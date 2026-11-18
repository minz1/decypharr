package manager

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type uncachedProvider struct {
	failingStatusProvider

	uncached bool
}

func (p *uncachedProvider) Config() config.Debrid {
	return config.Debrid{Name: "provider", Provider: "provider", DownloadUncached: p.uncached}
}

func arrChoice(category string, choice *bool) func(*config.Config) {
	return func(c *config.Config) {
		if choice != nil {
			c.Arrs = []config.Arr{{Name: category, DownloadUncached: choice}}
		}
	}
}

func TestUncachedChoiceIsTheEntrysForcedTrueElseTheArrsElseTheProviders(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	for name, tc := range map[string]struct {
		forced   bool
		arr      *bool
		provider bool
		want     bool
	}{
		"provider default allows":           {provider: true, want: true},
		"provider default refuses":          {provider: false, want: false},
		"arr false beats a provider's true": {arr: &no, provider: true, want: false},
		"arr true beats a provider's false": {arr: &yes, provider: false, want: true},
		"forced entry beats an arr's false": {forced: true, arr: &no, provider: false, want: true},
	} {
		provider := &uncachedProvider{uncached: tc.provider}
		m, entry := queuedEntryAddedAt(t, provider, time.Now())
		m.store = testConfigStore(t, arrChoice("books", tc.arr))
		entry.Category, entry.DownloadUncached = "books", tc.forced
		if got := m.uncachedAllowed(entry); got != tc.want {
			t.Errorf("%s: uncachedAllowed = %t, want %t", name, got, tc.want)
		}
		closePlacements(t, m)
	}
}

func uncachedFirstPoll(t *testing.T, choice *bool) entryMsg {
	t.Helper()
	provider := &uncachedProvider{
		phase: types.PhaseDownloading, cached: types.CachedNo,
		uncached: true,
	}
	m, entry := queuedEntryAddedAt(t, provider, time.Now())
	m.store = testConfigStore(t, arrChoice("books", choice))
	entry.Category = "books"
	msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry}}, false)
	closePlacements(t, m)
	return msg
}

func TestARecordWithNoStoredChoiceSurvivesItsFirstUncachedPoll(t *testing.T) {
	t.Parallel()
	if msg := uncachedFirstPoll(t, nil); msg.ev == nil || msg.ev.Observed != types.PhaseDownloading {
		t.Fatalf("first poll of an uncached download = %+v, want a downloading observation", msg.ev)
	}
}

func TestAnArrThatRefusesUncachedStillFailsTheFirstUncachedPoll(t *testing.T) {
	t.Parallel()
	no := false
	msg := uncachedFirstPoll(t, &no)
	if msg.ev == nil || msg.ev.Observed != types.PhaseFailed ||
		!strings.Contains(msg.ev.Reason, customerror.ErrTorrentNotCached.Error()) {
		t.Fatalf("first poll = %+v, want a not-cached failure", msg.ev)
	}
}

func TestAnExplicitUncachedRequestIsKeptOnTheQueuedEntry(t *testing.T) {
	t.Parallel()
	yes := true
	req := &ImportRequest{
		Magnet:           &utils.Magnet{InfoHash: "0123456789012345678901234567890123456789", Name: "release"},
		Arr:              arr.Arr{Name: "sonarr"},
		DownloadUncached: &yes,
	}
	if e := newTorrentQueueEntry(req, storage.EntryStatusQueued, config.WebDavUseFileName); !e.DownloadUncached {
		t.Fatal("a queued entry lost the request's download_uncached")
	}
}

func TestARebuiltRequestCarriesTheConfiguredChoice(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	for name, tc := range map[string]struct {
		forced bool
		arr    *bool
		want   *bool
	}{
		"no choice leaves the provider's": {},
		"arr false is kept":               {arr: &no, want: &no},
		"forced entry wins":               {forced: true, arr: &no, want: &yes},
	} {
		m := withTestConfig(t, &Manager{logger: zerolog.Nop()})
		m.store = testConfigStore(t, arrChoice("books", tc.arr))
		e := testEntry("a")
		e.Category, e.DownloadUncached = "books", tc.forced
		got := m.requestFromEntry(e).DownloadUncached
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: rebuilt override = %s, want %s", name, boolPtr(got), boolPtr(tc.want))
		}
	}
}

type probeCheckingProvider struct {
	uncachedProvider
}

func (p *probeCheckingProvider) Poll(
	_ context.Context,
	torrent *types.Torrent,
	_ bool,
) (*types.Torrent, types.Observation, error) {
	if !torrent.DownloadUncached {
		return torrent, types.Observation{}, fmt.Errorf("torrent %s: %w", torrent.Name, customerror.ErrTorrentNotCached)
	}
	return torrent, types.Observation{Phase: types.PhaseDownloading, Cached: types.CachedNo}, nil
}

func TestTheStatusProbeCarriesTheProvidersUncachedChoice(t *testing.T) {
	t.Parallel()
	provider := &probeCheckingProvider{uncachedProvider{uncached: true}}
	m, entry := queuedEntryAddedAt(t, provider, time.Now())
	msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry}}, false)
	closePlacements(t, m)
	if msg.ev == nil || msg.ev.Observed != types.PhaseDownloading {
		t.Fatalf("poll = %+v, want the provider asked with uncached allowed", msg.ev)
	}
}

func TestAnUncachedDownloadSurvivesTurningUncachedOffAfterItsSubmit(t *testing.T) {
	t.Parallel()
	provider := &uncachedProvider{
		phase: types.PhaseDownloading, cached: types.CachedNo,
		uncached: false,
	}
	m, entry := queuedEntryAddedAt(t, provider, time.Now())
	submitted := &types.Torrent{
		ID: "id", Debrid: "provider", InfoHash: entry.InfoHash, Name: "release",
		Status: types.TorrentStatusDownloading, DownloadUncached: true,
	}
	applyPayload(entry, submitResult(entry, submitted, nil), grab.PhaseSubmitting)
	msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry}}, false)
	closePlacements(t, m)
	if msg.ev == nil || msg.ev.Observed != types.PhaseDownloading {
		t.Fatalf("poll after uncached was turned off = %+v, want the submitted download kept", msg.ev)
	}
}

func boolPtr(b *bool) string {
	if b == nil {
		return "nil"
	}
	return strconv.FormatBool(*b)
}
