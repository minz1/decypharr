package manager

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type deadStatusProvider struct {
	throttlingProvider
}

func (p *deadStatusProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	return &types.Torrent{ID: "pid", Debrid: p.name, InfoHash: t.InfoHash, Name: t.Name}, nil
}

func (p *deadStatusProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	t.Status = types.TorrentStatusError
	return t, errors.New("torrent: x has error status dead")
}

func (*deadStatusProvider) DeleteTorrent(context.Context, string) error { return nil }

type failedPhaseProvider struct {
	deadStatusProvider

	checks atomic.Int64
}

func (p *failedPhaseProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	p.checks.Add(1)
	t.Status = types.TorrentStatusError
	return t, fmt.Errorf("torrent: %s has error status: dead: %w", t.Name, customerror.ErrReleaseFailed)
}

func failed() debrid.Client {
	return &failedPhaseProvider{deadStatusProvider: deadStatusProvider{throttlingProvider{name: "rd"}}}
}

func submitFailEffect(t *testing.T, providers ...debrid.Client) (grab.FailEffect, grab.Outcome) {
	t.Helper()
	return submitFailEffectOf(t, &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}}, providers...)
}

func submitFailEffectOf(t *testing.T, req *ImportRequest, providers ...debrid.Client) (grab.FailEffect, grab.Outcome) {
	t.Helper()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
	for _, p := range providers {
		m.clients.Store(p.Config().Name, p)
	}
	_, err := m.sendToDebrid(t.Context(), req, true)
	if err == nil {
		t.Fatal("submit succeeded, want a failure")
	}
	msg := submitResult(testEntry("hash"), nil, err)
	return msg.ev.Fail, msg.ev.Outcome
}

func refusing(name string, err error) debrid.Client {
	return &throttlingProvider{name: name, err: err}
}

func dead(name string) debrid.Client {
	return &deadStatusProvider{throttlingProvider{name: name}}
}

func TestASubmitEveryProviderRefusedFailsAsABadRelease(t *testing.T) {
	t.Parallel()
	transient := errors.New("giving up after 4 attempt(s)")
	for name, tc := range map[string]struct {
		providers []debrid.Client
		want      grab.FailEffect
	}{
		"not cached everywhere": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentNotCached), refusing("tb", customerror.ErrTorrentNotCached)},
			grab.FailBadRelease,
		},
		"blocked and not cached": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentBlocked), refusing("tb", customerror.ErrTorrentNotCached)},
			grab.FailBadRelease,
		},
		"blocked on the only provider":     {[]debrid.Client{refusing("rd", customerror.ErrTorrentBlocked)}, grab.FailBadRelease},
		"dead status on the only provider": {[]debrid.Client{dead("rd")}, grab.FailEntry},
		"not cached and transient": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentNotCached), refusing("tb", transient)},
			grab.FailEntry,
		},
		"failed on the only provider": {[]debrid.Client{failed()}, grab.FailBadRelease},
		"failed and not cached": {
			[]debrid.Client{failed(), refusing("tb", customerror.ErrTorrentNotCached)},
			grab.FailBadRelease,
		},
		"failed and transient": {
			[]debrid.Client{failed(), refusing("tb", customerror.NewProviderStatusError(errors.New("503"), 503))},
			grab.FailEntry,
		},
		"failed and hearsay skip": {
			[]debrid.Client{failed(), refusing("tb", hearsayNotCached("tb", "hash"))},
			grab.FailEntry,
		},
		"failed and not found": {
			[]debrid.Client{failed(), refusing("tb", customerror.ErrTorrentNotFound)},
			grab.FailEntry,
		},
		"failed and untyped status": {[]debrid.Client{failed(), dead("tb")}, grab.FailEntry},
		"dead status and transient": {[]debrid.Client{dead("rd"), refusing("tb", transient)}, grab.FailEntry},
		"not found everywhere": {
			[]debrid.Client{refusing("rd", customerror.ErrTorrentNotFound), refusing("tb", customerror.ErrTorrentNotFound)},
			grab.FailEntry,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, _ := submitFailEffect(t, tc.providers...); got != tc.want {
				t.Fatalf("submit fail effect = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAThrottledSubmitIsStillThrottled(t *testing.T) {
	t.Parallel()
	_, outcome := submitFailEffect(t,
		refusing("rd", customerror.ErrTorrentBlocked), refusing("tb", customerror.ErrTooManyActiveDownloads))
	if outcome != grab.OutcomeThrottled {
		t.Fatalf("outcome = %v, want throttled", outcome)
	}
}

func TestAThrottledSubmitWithAFailedProviderIsThrottled(t *testing.T) {
	t.Parallel()
	_, outcome := submitFailEffect(t, failed(), refusing("tb", customerror.ErrTooManyActiveDownloads))
	if outcome != grab.OutcomeThrottled {
		t.Fatalf("outcome = %v, want throttled", outcome)
	}
}

type acceptingProvider struct {
	throttlingProvider
}

func (p *acceptingProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	return &types.Torrent{ID: "ok", Debrid: p.name, InfoHash: t.InfoHash, Name: t.Name}, nil
}

func (p *acceptingProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	t.Status = types.TorrentStatusDownloaded
	return t, nil
}

func TestAFailedThenAcceptedSubmitPlaces(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
	rd := failed()
	m.clients.Store("rd", rd)
	m.clients.Store("tb", &acceptingProvider{throttlingProvider{name: "tb"}})
	got, err := m.sendToDebrid(t.Context(), &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Debrid != "tb" {
		t.Fatalf("placed at %q, want tb", got.Debrid)
	}
	if n := rd.(*failedPhaseProvider).checks.Load(); n != 1 {
		t.Fatalf("rd status checks = %d, want 1", n)
	}
}

func TestAFailedSubmitIsAVerdictWhateverItsName(t *testing.T) {
	t.Parallel()
	req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "Show Connection Reset By Peer.mkv"}}
	got, _ := submitFailEffectOf(t, req, failed())
	if got != grab.FailBadRelease {
		t.Fatalf("submit fail effect = %v, want %v", got, grab.FailBadRelease)
	}
}

type cuttingProvider struct {
	deadStatusProvider

	cancel context.CancelFunc
}

func (p *cuttingProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	p.cancel()
	return t, fmt.Errorf("torrent: %s has error status: dead: %w", t.Name, customerror.ErrReleaseFailed)
}

func TestAStopDuringAFailedCheckKeepsTheSubmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
	m.clients.Store("rd", &cuttingProvider{deadStatusProvider{throttlingProvider{name: "rd"}}, cancel})
	got, err := m.sendToDebrid(ctx, &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.TorrentStatusDownloading {
		t.Fatalf("status = %s, want downloading", got.Status)
	}
}

type timedOutFailedProvider struct {
	deadStatusProvider
}

func (p *timedOutFailedProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	return t, errors.Join(customerror.ErrProviderStatusTimeout, customerror.ErrReleaseFailed)
}

func TestAVerdictOutranksAStatusTimeout(t *testing.T) {
	t.Parallel()
	got, _ := submitFailEffect(t, &timedOutFailedProvider{deadStatusProvider{throttlingProvider{name: "rd"}}})
	if got != grab.FailBadRelease {
		t.Fatalf("submit fail effect = %v, want %v", got, grab.FailBadRelease)
	}
}

func TestTheHearsaySkipIsNoProviderVerdict(t *testing.T) {
	t.Parallel()
	if err := hearsayNotCached("rd", "hash"); submitVerdict(err) {
		t.Fatalf("hearsay skip %v counted as a provider verdict", err)
	}
}

func TestANotCachedPollFailsAsABadRelease(t *testing.T) {
	t.Parallel()
	provider := &uncachedProvider{
		phase: types.PhaseDownloading, cached: types.CachedNo,
	}
	m, entry := queuedEntryAddedAt(t, provider, time.Now())
	m.store = testConfigStore(t, nil)
	msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry}}, false)
	closePlacements(t, m)
	if msg.ev == nil || msg.ev.Fail != grab.FailBadRelease {
		t.Fatalf("not-cached poll = %+v, want FailBadRelease", msg.ev)
	}
}

type mutatingProvider struct {
	throttlingProvider

	cancel  context.CancelFunc
	check   func(*types.Torrent) error
	deletes atomic.Int64
}

func (p *mutatingProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	t.ID = "pid"
	t.InfoHash = "other"
	t.DownloadUncached = !t.DownloadUncached
	return t, nil
}

func (p *mutatingProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	if p.cancel != nil {
		p.cancel()
	}
	return t, p.check(t)
}

func (p *mutatingProvider) DeleteTorrent(context.Context, string) error {
	p.deletes.Add(1)
	return nil
}

type inputRecorder struct {
	throttlingProvider

	hash, status, name, link string
}

func (p *inputRecorder) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	p.hash, p.status, p.name, p.link = t.InfoHash, string(t.Status), t.Name, t.Magnet.Link
	return nil, customerror.ErrTorrentNotCached
}

func TestEachProviderAttemptGetsTheRequestTorrent(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
	m.clients.Store("rd", &mutatingProvider{
		name: "rd",
		check: func(torrent *types.Torrent) error {
			torrent.InfoHash = "other"
			torrent.Status = types.TorrentStatusError
			torrent.Name = "renamed"
			torrent.Magnet.Link = "magnet:other"
			return errors.New("torrent: x has error status dead")
		},
	})
	second := &inputRecorder{throttlingProvider: throttlingProvider{name: "tb"}}
	m.clients.Store("tb", second)
	req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name", Link: "magnet:request"}}
	if _, err := m.sendToDebrid(t.Context(), req, true); err == nil {
		t.Fatal("submit succeeded, want a failure")
	}
	if second.hash != "hash" || second.status != "" || second.name != "name" || second.link != "magnet:request" {
		t.Fatalf("second provider saw hash %q status %q name %q link %q, want the request's",
			second.hash, second.status, second.name, second.link)
	}
}

func TestAStoppedSubmitKeepsTheRequestHash(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
	m.clients.Store("rd", &mutatingProvider{
		name:   "rd",
		cancel: cancel,
		check:  func(*types.Torrent) error { return nil },
	})
	uncached := true
	req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}, DownloadUncached: &uncached}
	got, err := m.sendToDebrid(ctx, req, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.InfoHash != "hash" || !got.DownloadUncached {
		t.Fatalf("kept hash %q uncached %t, want hash and uncached", got.InfoHash, got.DownloadUncached)
	}
}

func TestADiscardedSubmitReleasesUnderTheRequestHash(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := withTestConfig(t, &Manager{
			logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client](), storage: newTestStore(t),
		})
		rd := &mutatingProvider{
			name: "rd",
			check: func(torrent *types.Torrent) error {
				return fmt.Errorf("torrent: %s has error status: dead: %w", torrent.Name, customerror.ErrReleaseFailed)
			},
		}
		m.clients.Store("rd", rd)
		seeded := &storage.Entry{
			InfoHash: "hash", Name: "name", State: storage.EntryStateDownloading, ActiveProvider: "rd",
			Providers: map[string]*storage.ProviderEntry{"rd": {Provider: "rd", ID: "pid"}},
		}
		if err := m.storage.AddQueue(seeded); err != nil {
			t.Fatal(err)
		}
		req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}}
		if _, err := m.sendToDebrid(t.Context(), req, true); err == nil {
			t.Fatal("submit succeeded, want a failure")
		}
		drainPlacements(t, m)
		if got := rd.deletes.Load(); got != 1 {
			t.Fatalf("provider deletes = %d, want 1", got)
		}
	})
}

func TestAKeptSubmitKeepsTheRequestHash(t *testing.T) {
	t.Parallel()
	for name, checkErr := range map[string]error{
		"status timeout":   customerror.ErrProviderStatusTimeout,
		"transient status": customerror.NewProviderStatusError(errors.New("503"), 503),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
			m.clients.Store("rd", &mutatingProvider{
				name:  "rd",
				check: func(*types.Torrent) error { return checkErr },
			})
			uncached := true
			req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}, DownloadUncached: &uncached}
			got, err := m.sendToDebrid(t.Context(), req, true)
			if err != nil {
				t.Fatal(err)
			}
			if got.InfoHash != "hash" || !got.DownloadUncached {
				t.Fatalf("kept hash %q uncached %t, want hash and uncached", got.InfoHash, got.DownloadUncached)
			}
		})
	}
}
