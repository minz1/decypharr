package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const recheckReason = "No provider can serve the torrent"

type staleOccurrenceError struct{}

func (*staleOccurrenceError) Error() string {
	return "the failure being delivered is no longer current"
}

type failDelivery struct {
	arr, provider, downloadID, entryID string
	name, reason, failReason           string
	evidence                           placementRef
	since                              time.Time
	precheck                           bool
	current                            func() bool
}

func (m *Manager) failerDeps(log zerolog.Logger) arrFailerDeps {
	return arrFailerDeps{
		now:            time.Now,
		startAttempt:   m.runWorkCtx,
		attemptGrab:    m.attemptFailGrab,
		attemptRecheck: m.attemptFailRecheck,
		attemptAck:     m.attemptFailAck,
		logger:         log,
	}
}

func (m *Manager) newFailer(log zerolog.Logger) *arrFailer {
	return newArrFailer(m.failerDeps(log))
}

func (m *Manager) tell(g grab.Grab) {
	if m.failer != nil {
		m.failer.wantGrab(g)
	}
}

func (m *Manager) untell(key string, at time.Time) {
	if m.failer != nil {
		m.failer.untell(key, at)
	}
}

func (m *Manager) deliverFail(ctx context.Context, recovery ArrRecovery, d failDelivery) (reacquire.Job, error) {
	if d.precheck {
		job, ok, err := recovery.FailReceipt(d.arr, d.downloadID, d.since)
		if err != nil {
			return reacquire.Job{}, fmt.Errorf("read the fail receipt in %s: %w", d.arr, err)
		}
		if ok {
			return job, nil
		}
	}
	release := func() { m.arr.ReleaseFailGrab(d.arr, d.downloadID, d.provider) }
	if err := m.arr.AdmitFailGrab(ctx, d.arr, d.downloadID, d.provider); err != nil {
		return reacquire.Job{}, err
	}
	if d.current != nil && !d.current() {
		release()
		return reacquire.Job{}, &staleOccurrenceError{}
	}
	if err := ctx.Err(); err != nil {
		release()
		return reacquire.Job{}, err
	}
	job, handed, err := recovery.FailDownload(d.arr, d.downloadID, d.entryID, d.since)
	if err != nil {
		release()
		return reacquire.Job{}, fmt.Errorf("fail download in %s: %w", d.arr, err)
	}
	if handed == reacquire.HandedExisting {
		release()
	} else {
		m.arr.CommitFailGrab(d.arr, d.downloadID, d.provider)
		m.logger.Info().
			Str("infohash", d.entryID).
			Str("name", d.name).
			Str("arr", d.arr).
			Str("job", job.ID).
			Str("provider", d.evidence.Provider).
			Str("placement_id", d.evidence.TorrentID).
			Str("fail_reason", d.failReason).
			Msg(d.reason + "; failing its grab in the Arr")
	}
	return job, nil
}

func (m *Manager) failGrab(entry *storage.Entry, recovery ArrRecovery, arrName, dropper string) error {
	_, err := m.deliverFail(m.work.Context(), recovery, failDelivery{
		arr: arrName, provider: failProvider(entry), downloadID: strings.ToUpper(entry.InfoHash),
		entryID: entry.InfoHash, name: entry.Name, reason: dropper + " dropped the torrent",
		evidence: placementRef{Provider: dropper},
	})
	return err
}

func activePlacement(entry *storage.Entry) placementRef {
	if p := entry.GetActiveProvider(); p != nil {
		return *placementOf(entry, p)
	}
	return placementRef{}
}

func (m *Manager) pendingFailAt(key string, at time.Time) (GrabView, bool) {
	v, ok := m.grabs.get(key)
	if !ok || v.State.Phase != grab.PhaseFailed || v.State.ArrFail.State != grab.ArrFailPending ||
		!v.State.Failure.At.Equal(at) {
		return GrabView{}, false
	}
	return v, true
}

func failResultOf(res attemptResult, err error) attemptResult {
	var held *arr.HeldError
	var other *reacquire.OtherJobError
	var stale *staleOccurrenceError
	res.err = err
	switch {
	case errors.As(err, &held):
		res.kind, res.retryAt = resHeld, held.RetryAt
	case errors.As(err, &other):
		res.kind = resOtherJob
	case errors.As(err, &stale):
		res.kind, res.err = resStale, nil
	default:
		res.kind = resError
	}
	return res
}

func (m *Manager) attemptFailGrab(ctx context.Context, spec attemptSpec) attemptResult {
	res := attemptResult{spec: spec}
	view, ok := m.pendingFailAt(spec.key, spec.at)
	if !ok {
		res.kind = resStale
		return res
	}
	recovery := m.recoveryService()
	if recovery == nil {
		res.kind, res.err = resError, errors.New("arr recovery service is not running")
		return res
	}
	if _, registered := m.Arr().Get(spec.arr); !registered {
		res.kind = resNoArr
		return res
	}
	job, err := m.deliverFail(ctx, recovery, failDelivery{
		arr: spec.arr, provider: spec.provider, downloadID: strings.ToUpper(spec.key), entryID: view.Entry.InfoHash,
		name: view.Entry.Name, reason: "Provider rejected the torrent", since: spec.at, precheck: true,
		evidence: activePlacement(&view.Entry), failReason: view.State.Failure.Reason,
		current: func() bool { _, current := m.pendingFailAt(spec.key, spec.at); return current },
	})
	if err != nil {
		return failResultOf(res, err)
	}
	return m.ackFailQueued(ctx, spec, job.ID)
}

func (m *Manager) attemptFailAck(ctx context.Context, spec attemptSpec) attemptResult {
	return m.ackFailQueued(ctx, spec, spec.jobID)
}

func (m *Manager) ackFailQueued(ctx context.Context, spec attemptSpec, jobID string) attemptResult {
	res := attemptResult{spec: spec, jobID: jobID, kind: resAcked}
	err := m.askEntry(
		ctx,
		spec.key,
		eventMsg(grab.Event{Kind: grab.EvArrFailQueued, Occurrence: spec.at, JobID: jobID}),
	)
	if err != nil {
		res.kind, res.err = resAckFailed, err
	}
	return res
}

func (m *Manager) recheckTarget(entry *storage.Entry) (string, bool) {
	if !entry.IsTorrent() || entry.Manual {
		return "", false
	}
	instance, ok := m.Arr().Get(entry.Category)
	return instance.Name, ok
}

func (m *Manager) wantRecheck(entry *storage.Entry) {
	if m.failer == nil {
		return
	}
	if arrName, ok := m.recheckTarget(entry); ok {
		m.failer.wantRecheck(entry, arrName)
	}
}

func (m *Manager) attemptFailRecheck(ctx context.Context, spec attemptSpec) attemptResult {
	res := attemptResult{spec: spec}
	entry := m.badEntry(spec.key)
	if entry == nil {
		res.kind = resStale
		return res
	}
	arrName, ok := m.recheckTarget(entry)
	if !ok {
		res.kind = resStale
		return res
	}
	if bucket := failBucket(arrName, failProvider(entry)); bucket != spec.bucket() {
		res.kind, res.arr, res.provider = resMoved, arrName, failProvider(entry)
		return res
	}
	recovery := m.recoveryService()
	if recovery == nil {
		res.kind, res.err = resError, errors.New("arr recovery service is not running")
		return res
	}
	records, err := m.Arr().Queue(ctx, arrName)
	if err != nil {
		res.kind, res.err = resError, fmt.Errorf("arr queue unreadable; leaving the grab of a bad entry: %w", err)
		return res
	}
	if !slices.ContainsFunc(records, func(record arr.QueueSchema) bool {
		return strings.EqualFold(record.DownloadID, entry.InfoHash)
	}) {
		res.kind = resStale
		return res
	}
	_, err = m.deliverFail(ctx, recovery, failDelivery{
		arr: arrName, provider: spec.provider, downloadID: strings.ToUpper(entry.InfoHash), entryID: entry.InfoHash,
		name: entry.Name, reason: recheckReason,
		current: func() bool {
			fresh := m.badEntry(spec.key)
			if fresh == nil || !fresh.CreatedAt.Equal(entry.CreatedAt) {
				return false
			}
			freshArr, _ := m.recheckTarget(fresh)
			return freshArr == arrName
		},
	})
	if err != nil {
		return failResultOf(res, err)
	}
	res.kind = resAcked
	return res
}
