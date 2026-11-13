package manager

import (
	"context"
	"errors"
	"fmt"
	"testing"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestSubmitResultMapsTheProviderAnswer(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{InfoHash: "h", Files: map[string]*storage.File{}}
	linked := map[string]debridTypes.File{"a.mkv": {Name: "a.mkv", Link: "https://x/a"}}
	cases := []struct {
		name       string
		t          *debridTypes.Torrent
		err        error
		outcome    grab.Outcome
		fail       grab.FailEffect
		downloaded bool
		linked     bool
	}{
		{name: "throttled", err: &throttledError{providers: []string{"rd"}, err: errors.New("cap")},
			outcome: grab.OutcomeThrottled},
		{name: "refused", err: errors.New("bad magnet"), outcome: grab.OutcomePermanent, fail: grab.FailEntry},
		{
			name:    "downloading",
			t:       &debridTypes.Torrent{ID: "1", Debrid: "rd", Status: debridTypes.TorrentStatusDownloading},
			outcome: grab.OutcomePlacement,
			linked:  true,
		},
		{name: "cached", t: &debridTypes.Torrent{ID: "1", Debrid: "rd", Status: debridTypes.TorrentStatusDownloaded,
			Files: linked}, outcome: grab.OutcomePlacement, downloaded: true, linked: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := submitResult(entry, c.t, c.err)
			if m.ev == nil || m.ev.Kind != grab.EvSubmitResult {
				t.Fatalf("event = %+v, want a submit result", m.ev)
			}
			ev := *m.ev
			if ev.Outcome != c.outcome || ev.Fail != c.fail || ev.Downloaded != c.downloaded ||
				ev.AllLinked != c.linked {
				t.Errorf("event = %+v", ev)
			}
			if c.outcome == grab.OutcomeThrottled && (len(m.providers) != 1 || m.providers[0] != "rd") {
				t.Errorf("providers = %v, want [rd]", m.providers)
			}
			if c.outcome == grab.OutcomePlacement && (ev.Provider != "rd" || ev.TorrentID != "1" || m.torrent != c.t) {
				t.Errorf("placement = %s/%s torrent %v", ev.Provider, ev.TorrentID, m.torrent)
			}
		})
	}
}

func TestActionResultRetriesAStopAndFailsAnError(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want grab.EventKind
		none bool
	}{
		{name: "done", ctx: t.Context(), want: grab.EvActionDone},
		{name: "mount stopping", ctx: t.Context(), err: fmt.Errorf("symlink: %w", ErrMountStopping), none: true},
		{name: "stopped", ctx: cancelled, err: context.Canceled, none: true},
		{name: "failed", ctx: t.Context(), err: errors.New("disk full"), want: grab.EvActionFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := actionResult(c.ctx, &storage.Entry{Name: "n"}, c.err)
			if c.none {
				if m.ev != nil {
					t.Fatalf("event = %+v, want none so the action runs again", m.ev)
				}
				return
			}
			if m.ev == nil || m.ev.Kind != c.want {
				t.Fatalf("event = %+v, want %s", m.ev, c.want)
			}
			if c.want == grab.EvActionFailed && m.ev.Fail != grab.FailEntry {
				t.Errorf("fail = %v, want FailEntry", m.ev.Fail)
			}
		})
	}
}

func TestPolledUpdateKeepsTheFirstDownloadedAt(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{InfoHash: "h", ActiveProvider: "rd", Files: map[string]*storage.File{},
		Providers: map[string]*storage.ProviderEntry{"rd": {Provider: "rd", ID: "1"}}}
	unlinked := map[string]debridTypes.File{"a.mkv": {Name: "a.mkv"}}
	t1 := &debridTypes.Torrent{ID: "1", Debrid: "rd", Status: debridTypes.TorrentStatusDownloaded, Progress: 100,
		Files: unlinked}
	m := polled(entry, entry.Providers["rd"], t1, observationOfStatus(t1))
	if m.ev == nil || !m.ev.Downloaded || m.ev.AllLinked {
		t.Fatalf("event = %+v, want downloaded and awaiting links", m.ev)
	}
	m.update(entry)
	first := entry.Providers["rd"].DownloadedAt
	if first == nil || entry.Progress != 1 {
		t.Fatalf("after update: downloadedAt %v progress %v", first, entry.Progress)
	}
	m.update(entry)
	if entry.Providers["rd"].DownloadedAt != first {
		t.Errorf("a second update moved DownloadedAt; the link wait would restart")
	}
}

func TestApplyParsedNZBTwiceGivesTheSameRecord(t *testing.T) {
	t.Parallel()
	nzb := &storage.NZB{ID: "n1", Name: "Show", TotalSize: 10,
		Files: []storage.NZBFile{{Name: "a.mkv", Size: 10}}}
	entry := &storage.Entry{InfoHash: "h", Magnet: "/staged.nzb", Files: map[string]*storage.File{}}
	applyParsedNZB(entry, nzb)
	applyParsedNZB(entry, nzb)
	if entry.Magnet != "" || entry.ActiveProvider != usenetProvider || len(entry.Files) != 1 || entry.Progress != 1 {
		t.Errorf("entry = magnet %q provider %q files %d progress %v", entry.Magnet, entry.ActiveProvider,
			len(entry.Files), entry.Progress)
	}
	if p := entry.GetActiveProvider(); p == nil || p.DownloadedAt == nil || p.ID != "n1" {
		t.Errorf("placement = %+v", p)
	}
}

func TestPollSendsTheObservedPhase(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{InfoHash: "h", ActiveProvider: "rd", Files: map[string]*storage.File{},
		Providers: map[string]*storage.ProviderEntry{"rd": {Provider: "rd", ID: "1"}}}
	torrent := func(status debridTypes.TorrentStatus, seeders int, progress float64) *debridTypes.Torrent {
		return &debridTypes.Torrent{ID: "1", Debrid: "rd", Status: status, Seeders: seeders, Progress: progress}
	}
	cases := []struct {
		name       string
		t          *debridTypes.Torrent
		want       debridTypes.Phase
		raw        string
		downloaded bool
	}{
		{"idle", torrent(debridTypes.TorrentStatusRemoteQueued, 0, 0), debridTypes.PhaseStalled, "", false},
		{"seeded", torrent(debridTypes.TorrentStatusRemoteQueued, 2, 0), debridTypes.PhaseRemoteQueued, "", false},
		{"progressed", torrent(debridTypes.TorrentStatusRemoteQueued, 0, 10), debridTypes.PhaseRemoteQueued, "", false},
		{"downloading", torrent(debridTypes.TorrentStatusDownloading, 0, 0), debridTypes.PhaseDownloading, "", false},
		{"downloaded", torrent(debridTypes.TorrentStatusDownloaded, 0, 100), debridTypes.PhaseDownloaded, "", true},
		{"unknown", torrent("magnet_conversion", 1, 0), debridTypes.PhaseUnknown, "magnet_conversion", false},
	}
	for i := range cases {
		c := &cases[i]
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			msg := polled(entry, entry.Providers["rd"], c.t, observationOfStatus(c.t))
			ev := msg.ev
			if ev == nil || ev.Kind != grab.EvProviderObserved || ev.Observed != c.want || ev.Raw != c.raw ||
				ev.Downloaded != c.downloaded || !ev.AllLinked {
				t.Fatalf("event = %+v, want %v raw %q downloaded %v all linked", ev, c.want, c.raw, c.downloaded)
			}
			if msg.seen == nil || *msg.seen != (pollSeen{}) {
				t.Fatalf("seen = %+v, want the zero pollSeen that clears a pending wait", msg.seen)
			}
		})
	}
}

func observationOfStatus(t *debridTypes.Torrent) debridTypes.Observation {
	o := debridTypes.Observation{Provider: t.Debrid, ID: t.ID, Progress: t.Progress, Speed: t.Speed, Seeders: t.Seeders}
	switch t.Status {
	case debridTypes.TorrentStatusDownloaded:
		o.Phase = debridTypes.PhaseDownloaded
	case debridTypes.TorrentStatusError:
		o.Phase = debridTypes.PhaseFailed
	case debridTypes.TorrentStatusDownloading:
		o.Phase = debridTypes.PhaseDownloading
	case debridTypes.TorrentStatusRemoteQueued:
		o.Phase, o.Stallable = debridTypes.PhaseRemoteQueued, true
	default:
		o.Phase, o.Raw = debridTypes.PhaseUnknown, string(t.Status)
	}
	return debridTypes.WithStall(o)
}
