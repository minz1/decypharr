package manager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// entryChild builds the work of one child of a grab. A child reports one message and writes neither the queue
// record nor the library: the actor applies what it reports.
func (m *Manager) entryChild(call entryChildCall) func(ctx context.Context) entryMsg {
	switch call.kind {
	case grab.ChildSubmit:
		if call.grab.State.NZB {
			return func(ctx context.Context) entryMsg { return m.parseChild(ctx, call) }
		}
		return func(ctx context.Context) entryMsg { return m.submitChild(ctx, call) }
	case grab.ChildPoll:
		return func(ctx context.Context) entryMsg { return m.pollChild(ctx, call, false) }
	case grab.ChildLinks:
		return func(ctx context.Context) entryMsg { return m.pollChild(ctx, call, true) }
	case grab.ChildAction:
		return func(ctx context.Context) entryMsg { return m.actionChild(ctx, call) }
	case grab.ChildCleanup:
		return func(ctx context.Context) entryMsg { return m.cleanupChild(ctx, call) }
	case grab.ChildLookup:
		return func(ctx context.Context) entryMsg { return m.lookupChild(ctx, call) }
	}
	return func(context.Context) entryMsg { return entryMsg{} }
}

// submitChild sends the grab's magnet to a provider. A stop, or a provider's limit that cut the submit before it
// answered, says nothing about the submit: the child reports no result and the grab looks the submit up.
func (m *Manager) submitChild(ctx context.Context, call entryChildCall) entryMsg {
	req := call.req
	if req == nil {
		req = m.requestFromEntry(call.grab.Entry)
	}
	t, err := m.sendToDebrid(ctx, req, call.grab.Entry.Manual)
	if err != nil && (stopped(ctx) || errors.Is(err, errSubmitOutcomeUnknown)) {
		return entryMsg{}
	}
	return submitResult(call.grab.Entry, t, err)
}

// submitResult is the event a provider's answer to a submit makes.
func submitResult(entry *storage.Entry, t *debridTypes.Torrent, err error) entryMsg {
	if throttled, ok := errors.AsType[*throttledError](err); ok {
		m := eventMsg(grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomeThrottled})
		m.providers = throttled.providers
		return m
	}
	if err != nil {
		fail := grab.FailEntry
		if errors.Is(err, errReleaseRefused) {
			fail = grab.FailBadRelease
		}
		return eventMsg(grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomePermanent, Fail: fail,
			Reason: "submit failed: " + err.Error()})
	}
	m := eventMsg(grab.Event{
		Kind: grab.EvSubmitResult, Outcome: grab.OutcomePlacement, Provider: t.Debrid, TorrentID: t.ID,
		Downloaded: t.Status == debridTypes.TorrentStatusDownloaded, AllLinked: !awaitingLinks(entry, t),
	})
	m.torrent = t
	m.update = func(e *storage.Entry) { e.DownloadUncached = e.DownloadUncached || t.DownloadUncached }
	return m
}

// parseChild parses and processes a grab's staged NZB: for an NZB, that is its submit.
func (m *Manager) parseChild(ctx context.Context, call entryChildCall) entryMsg {
	entry := call.grab.Entry
	if m.usenet == nil {
		return parseFailed(errors.New("usenet is not configured"))
	}
	content, err := os.ReadFile(entry.Magnet)
	if err != nil {
		return parseFailed(fmt.Errorf("read staged NZB: %w", err))
	}
	name := cmp.Or(entry.OriginalFilename, entry.Name)
	parsed, err := m.parseNZB(ctx, entry.InfoHash, name, content, entry.Category)
	subject := hearsay.NZBSubjectFromGroups(parsed.Groups)
	useHint := entry.Manual
	switch {
	case stopped(ctx):
		return entryMsg{}
	case errors.Is(err, errParseBudget):
		m.logger.Warn().
			Err(err).
			Str("name", entry.Name).
			Msg("NZB parse ran out of time; the grab waits and tries again")
		return eventMsg(grab.Event{Kind: grab.EvParseResult, Outcome: grab.OutcomeThrottled})
	case err != nil:
		cause := m.parseFailCause(parsed.Outcome, content)
		m.logger.Warn().
			Err(err).
			Str("name", entry.Name).
			Object("outcome", parsed.Outcome).
			Msg("NZB parse failed")
		if m.hearsay != nil && cause != "" {
			m.hearsay.ReportNZB(subject, false)
		}
		return parseFailedWith(fmt.Errorf("usenet parse failed: %w", err), cause)
	case useHint && m.hearsay != nil && m.hearsay.NZBClaimedIncomplete(subject):
		return parseFailed(errors.New("nzb rejected: hearsay claims segments missing on every configured backbone"))
	}
	if !useHint && m.hearsay != nil && m.hearsay.NZBClaimedIncomplete(subject) {
		m.logger.Info().
			Str("subject", subject).
			Str("category", entry.Category).
			Msg("Hearsay hint bypassed: the provider answers")
	}
	processCtx, cancel := context.WithTimeout(ctx, m.usenetTimeout)
	defer cancel()
	process := m.processNZBFunc
	if process == nil {
		process = m.usenet.Process
	}
	processed, result, err := process(processCtx, parsed)
	switch {
	case stopped(ctx):
		return entryMsg{}
	case err != nil:
		m.logger.Warn().
			Err(err).
			Str("name", entry.Name).
			Object("outcome", result.Outcome).
			Bool("gate_missing", result.GateMissing).
			Msg("NZB process failed")
		if result.GateMissing {
			m.hearsay.ReportNZB(subject, false)
		}
		return parseFailedWith(fmt.Errorf("failed to process nzb: %w", err), m.parseFailCause(result.Outcome, content))
	case len(processed.Files) == 0:
		return parseFailed(errors.New("nzb has no files"))
	}
	m.hearsay.ReportNZB(subject, true)
	m.usenet.RemoveStagedNZB(entry.Magnet)
	msg := eventMsg(grab.Event{Kind: grab.EvParseResult, Outcome: grab.OutcomePlacement, Provider: usenetProvider,
		TorrentID: processed.ID, Downloaded: true, AllLinked: true})
	msg.update = func(e *storage.Entry) { applyParsedNZB(e, processed) }
	return msg
}

func parseFailed(err error) entryMsg { return parseFailedWith(err, "") }

func parseFailedWith(err error, cause string) entryMsg {
	return eventMsg(grab.Event{Kind: grab.EvParseResult, Outcome: grab.OutcomePermanent, Fail: grab.FailEntry,
		Reason: err.Error(), Cause: cause})
}

// applyParsedNZB records a processed NZB on its grab's record; it may run twice (a save retried).
func applyParsedNZB(e *storage.Entry, nzb *storage.NZB) {
	e.Magnet = ""
	e.Name, e.OriginalFilename = nzb.Name, nzb.Name
	e.Size, e.Bytes = nzb.TotalSize, nzb.TotalSize
	e.Status = storage.EntryStatusDownloading
	e.SetActive(usenetProvider, true)
	placement := e.AddUsenetProvider(nzb)
	for _, file := range nzb.Files {
		e.Files[file.Name] = &storage.File{Name: file.Name, Size: file.Size, InfoHash: e.InfoHash, AddedOn: e.AddedOn}
	}
	if placement.DownloadedAt == nil {
		now := time.Now()
		placement.DownloadedAt = &now
	}
	placement.Progress = 1
	e.Progress = 1
}

// pollChild checks the grab's placement once. links is the AwaitingLinks check: it reports only when the links are
// ready or never will be, and a check that says nothing new reports no result, so the actor checks again.
func (m *Manager) pollChild(ctx context.Context, call entryChildCall, links bool) entryMsg {
	entry := call.grab.Entry
	placement := entry.GetActiveProvider()
	if placement == nil {
		return eventMsg(
			grab.Event{Kind: grab.EvPlacementGone, Fail: grab.FailEntry, Reason: "no active placement found"},
		)
	}
	client := m.ProviderClient(entry.ActiveProvider)
	if client == nil {
		return eventMsg(grab.Event{Kind: grab.EvPlacementGone, Fail: grab.FailEntry,
			Reason: "debrid client not found: " + entry.ActiveProvider})
	}
	t, obs, err := client.Poll(ctx, m.statusProbe(entry, placement), call.allowSelect)
	if stopped(ctx) {
		return entryMsg{}
	}
	wait := providerWaitOfPhase(obs.Phase)
	if err != nil {
		wait = providerWaitOf(err)
	}
	if wait != waitNone || (err != nil && transientStatusError(err)) {
		m.logPendingStatus(entry, placement, wait, err)
		msg := pendingPoll(wait)
		if links && err == nil && obs.LinksPending {
			msg.linksMissing = placementOf(entry, placement)
		}
		return msg
	}
	m.queue.waits.observe(entry.InfoHash, waitNone)
	switch {
	case err != nil || t == nil:
		return m.placementFailed(ctx, entry, placement, t, err)
	case obs.Phase == debridTypes.PhaseFailed:
		t.Status = debridTypes.TorrentStatusError
		return m.placementFailed(ctx, entry, placement, t,
			fmt.Errorf("torrent %s has error status: %s", t.Name, obs.Raw))
	case debridTypes.NotCached(obs, m.uncachedAllowed(entry)):
		return m.placementFailed(ctx, entry, placement, t,
			fmt.Errorf("torrent %s: %w", t.Name, customerror.ErrTorrentNotCached))
	case obs.Phase == debridTypes.PhaseAwaitingSelection && !call.allowSelect:
		m.slotFreed(entry.ActiveProvider)
		return eventMsg(grab.Event{Kind: grab.EvProviderObserved, Observed: debridTypes.PhaseFailed,
			Fail: grab.FailNoEffect, Reason: "provider never accepted the file selection"})
	case links:
		if obs.Phase != debridTypes.PhaseDownloaded {
			return entryMsg{}
		}
		return m.linksChecked(entry, placement, t)
	}
	if obs.Phase == debridTypes.PhaseDownloaded && !awaitingLinks(entry, t) {
		m.slotFreed(entry.ActiveProvider)
	}
	msg := polled(entry, placement, t, obs)
	msg.nudged = obs.Nudged
	return msg
}

// pendingPoll is what a status check left for the next poll reports: what the provider is still doing with the
// placement, which the grab's deadline for that wait reads. Any other transient error reports nothing.
func pendingPoll(wait providerWait) entryMsg {
	switch wait {
	case waitConverting:
		return entryMsg{seen: &pollSeen{pending: grab.PendingConverting, dead: true}}
	case waitPending:
		return entryMsg{seen: &pollSeen{pending: grab.PendingProvider}}
	case waitNone:
	}
	return entryMsg{}
}

// logPendingStatus logs a status check left for the next poll, as retryNextPass does.
func (m *Manager) logPendingStatus(
	entry *storage.Entry,
	placement *storage.ProviderEntry,
	wait providerWait,
	err error,
) {
	if wait == waitNone {
		m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Status check failed; retrying on the next poll")
		return
	}
	line := m.logger.Debug()
	if m.queue.waits.observe(entry.InfoHash, wait) != wait {
		line = m.logger.Warn()
	}
	line.Err(err).Str("name", entry.Name).Stringer("wait", wait).Dur("waited", providerWaitAge(entry, placement)).
		Msg("Entry waiting on the provider; checking again on the next poll")
}

// placementFailed is the event of a placement the provider failed or lost; its torrent is deleted there.
func (m *Manager) placementFailed(
	ctx context.Context,
	entry *storage.Entry,
	placement *storage.ProviderEntry,
	t *debridTypes.Torrent,
	err error,
) entryMsg {
	m.slotFreed(entry.ActiveProvider)
	fail := grab.FailEntry
	if releaseIsDead(t, err) {
		fail = grab.FailBadRelease
	}
	if err != nil {
		m.logStatusFailure(entry, placement, err)
		if t != nil && t.ID != "" {
			provider, id := entry.ActiveProvider, t.ID
			_ = m.discardSubmitted(ctx, provider, id, entry.InfoHash)
		}
	}
	switch {
	case err != nil:
		return eventMsg(
			grab.Event{
				Kind:     grab.EvProviderObserved,
				Observed: debridTypes.PhaseFailed,
				Fail:     fail,
				Reason:   err.Error(),
			},
		)
	case t == nil:
		return eventMsg(grab.Event{Kind: grab.EvPlacementGone, Fail: grab.FailBadRelease,
			Reason: "debrid entry not found"})
	}
	m.logger.Error().Str("debrid", t.Debrid).Str("name", t.Name).Str("status", string(t.Status)).
		Msg("Entry in error state")
	return eventMsg(
		grab.Event{Kind: grab.EvProviderObserved, Observed: debridTypes.PhaseFailed, Fail: grab.FailBadRelease,
			Reason: "entry in error state on debrid: " + t.Debrid},
	)
}

// polled is the observation a status check of a placement makes; it carries the placement's progress.
func polled(
	entry *storage.Entry,
	placement *storage.ProviderEntry,
	t *debridTypes.Torrent,
	obs debridTypes.Observation,
) entryMsg {
	downloaded := obs.Phase == debridTypes.PhaseDownloaded
	msg := eventMsg(grab.Event{
		Kind: grab.EvProviderObserved, Observed: obs.Phase, Raw: obs.Raw, Downloaded: downloaded,
		AllLinked: !awaitingLinks(entry, t),
	})
	msg.seen = &pollSeen{}
	provider, id := placement.Provider, placement.ID
	progress, speed, size, seeders := t.Progress/percentScale, t.Speed, t.GetSize(), t.Seeders
	msg.update = func(e *storage.Entry) {
		e.Progress, e.Speed, e.Size, e.Seeders = progress, speed, size, seeders
		e.UpdatedAt = time.Now()
		p := e.Providers[provider]
		if p == nil || p.ID != id {
			return
		}
		p.Progress = progress
		if downloaded && p.DownloadedAt == nil {
			now := time.Now()
			p.DownloadedAt = &now
		}
		if downloaded && !awaitingLinks(e, t) {
			refreshFinishedPlacement(e, p, t)
		}
	}
	return msg
}

// linksChecked reports links that are ready, or a wait for them that ran out; else nothing, so the actor asks again.
func (m *Manager) linksChecked(
	entry *storage.Entry,
	placement *storage.ProviderEntry,
	t *debridTypes.Torrent,
) entryMsg {
	if !awaitingLinks(entry, t) {
		m.slotFreed(entry.ActiveProvider)
		msg := eventMsg(grab.Event{Kind: grab.EvLinksReady})
		provider, id := placement.Provider, placement.ID
		msg.update = func(e *storage.Entry) {
			if p := e.Providers[provider]; p != nil && p.ID == id {
				refreshFinishedPlacement(e, p, t)
			}
		}
		return msg
	}
	m.logger.Debug().Str("name", entry.Name).Msg("Downloaded, waiting for file links before the action")
	return entryMsg{linksMissing: placementOf(entry, placement)}
}

// actionChild runs the grab's post-download action. A stop of the mount or the manager cuts it short with no
// result, so the actor runs it again; any other error fails the grab.
func (m *Manager) actionChild(ctx context.Context, call entryChildCall) entryMsg {
	entry := call.grab.Entry
	m.logger.Info().Str("name", entry.Name).Str("action", string(entry.Action)).
		Msg("Download completed, processing action")
	m.InvalidateEntryCache()
	if err := m.RefreshMount(); err != nil {
		m.logger.Error().Err(err).Msg("Mount refresh failed")
	}
	if call.grab.State.PackKey == "" {
		if seasons := m.downloader.seasonsOf(entry); len(seasons) > 0 {
			return m.packChild(ctx, call, seasons)
		}
	}
	owned := withOwner(ctx, &actionOwner{progress: m.progressSink(call.grab.State.Key)})
	result := actionResult(ctx, entry, m.downloader.downloadIn(owned, entry, m.actionMountPath(call.grab)))
	if result.ev != nil && result.ev.Kind == grab.EvActionDone {
		path := entry.DownloadPath(m.folderNaming())
		result.update = func(e *storage.Entry) { e.ContentPath = path }
	}
	return result
}

// actionResult is the event an action's end makes.
func actionResult(ctx context.Context, entry *storage.Entry, err error) entryMsg {
	switch {
	case err == nil:
		return eventMsg(grab.Event{Kind: grab.EvActionDone})
	case errors.Is(err, ErrMountStopping), stopped(ctx):
		return entryMsg{}
	}
	return eventMsg(grab.Event{Kind: grab.EvActionFailed, Fail: grab.FailEntry,
		Reason: fmt.Sprintf("post-download action of %s failed: %v", entry.Name, err)})
}

// cleanupChild releases what a deleted or expired grab holds: placements that landed after the delete, its
// download at the provider when asked, its staged NZB and, when asked, its files. A file it cannot remove leaves
// no result, so the actor tries again.
func (m *Manager) cleanupChild(ctx context.Context, call entryChildCall) entryMsg {
	entry := call.grab.Entry
	for _, ref := range call.releases {
		if err := m.discardSubmitted(ctx, ref.Provider, ref.TorrentID, call.grab.State.Key); err != nil {
			return entryMsg{}
		}
	}
	if entry == nil {
		return eventMsg(grab.Event{Kind: grab.EvCleanupDone})
	}
	m.queue.waits.observe(entry.InfoHash, waitNone)
	if call.cleanup.ReleaseDelete {
		if err := m.stopProviderDownload(ctx, entry); err != nil {
			return entryMsg{}
		}
	}
	remove := m.queue.removeStagedNZB
	if call.cleanup.DeleteFiles {
		remove = m.queue.deleteEntryFiles
	}
	if err := remove(entry); err != nil {
		m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Failed to remove a deleted grab's files; retrying")
		return entryMsg{}
	}
	return eventMsg(grab.Event{Kind: grab.EvCleanupDone})
}

// requestFromEntry rebuilds the add request of a queued torrent that was never placed at a provider.
func (m *Manager) requestFromEntry(entry *storage.Entry) *ImportRequest {
	magnet, err := utils.GetMagnetInfo(entry.Magnet, m.store.Get().AlwaysRmTrackerUrls)
	if err != nil {
		magnet = utils.ConstructMagnet(entry.InfoHash, entry.Name)
	}
	req := NewTorrentRequest(
		entry.ActiveProvider,
		m.store.Get().DownloadFolder,
		magnet,
		m.arr.GetOrCreate(entry.Category),
		entry.Action,
		m.downloadUncachedOverride(entry),
		entry.CallbackURL,
		ImportTypeAPI,
		entry.SkipMultiSeason,
	)
	req.ID = entry.InfoHash
	req.Manual = entry.Manual
	return req
}

// actionMountPath is where a grab's files are read: a season reads its pack's folder on the mount.
func (m *Manager) actionMountPath(g grab.Grab) string {
	if g.State.PackKey != "" {
		if pack, err := m.storage.GetQueued(g.State.PackKey); err == nil {
			return m.GetTorrentMountPath(pack)
		}
	}
	return m.GetTorrentMountPath(g.Entry)
}

// packChild fans a season pack out: it hands each season that is not complete to its own actor and reports the
// seasons it started. A season the actor refuses ends the attempt with no result, so the pack's action runs again.
func (m *Manager) packChild(ctx context.Context, call entryChildCall, seasons []SeasonInfo) entryMsg {
	pack, packKey := call.grab.Entry, call.grab.State.Key
	started := make([]int, 0, len(seasons))
	for i, season := range convertToMultiSeason(pack, seasons) {
		if ctx.Err() != nil {
			return entryMsg{}
		}
		started = append(started, seasons[i].SeasonNumber)
		if saved, err := m.queue.GetTorrent(season.InfoHash); err == nil && saved.IsComplete {
			continue
		}
		msg := eventMsg(grab.Event{Kind: grab.EvSubmit, PackKey: packKey})
		msg.entry = season
		if err := m.entries.send(strings.ToLower(season.InfoHash), msg); err != nil {
			m.logger.Warn().Err(err).Str("name", season.Name).Msg("Season not handed on; the pack tries again")
			return entryMsg{}
		}
	}
	m.logger.Info().Str("name", pack.Name).Ints("seasons", started).Msg("Multi-season torrent fanned out")
	return eventMsg(grab.Event{Kind: grab.EvSeasonsStarted, Seasons: started})
}

func (m *Manager) downloadUncachedOverride(entry *storage.Entry) *bool {
	if entry.DownloadUncached {
		forced := true
		return &forced
	}
	for _, a := range m.store.Get().Arrs {
		if a.Name == entry.Category && a.DownloadUncached != nil {
			choice := *a.DownloadUncached
			return &choice
		}
	}
	return nil
}

func (m *Manager) uncachedAllowed(entry *storage.Entry) bool {
	if override := m.downloadUncachedOverride(entry); override != nil {
		return *override
	}
	if client := m.ProviderClient(entry.ActiveProvider); client != nil {
		return client.Config().DownloadUncached
	}
	return false
}

func placementOf(entry *storage.Entry, placement *storage.ProviderEntry) *placementRef {
	return &placementRef{Provider: entry.ActiveProvider, TorrentID: placement.ID}
}
