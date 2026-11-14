package qbit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func addGrab(t *testing.T, mgr *manager.Manager, hash string, st grab.State) {
	t.Helper()
	e := &storage.Entry{
		InfoHash: hash, Name: hash, Category: "sonarr", Protocol: config.ProtocolTorrent,
		Size: 100, Progress: 0.4,
	}
	switch st.Phase {
	case grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseComplete:
		if st.PackKey == "" {
			e.Providers = map[string]*storage.ProviderEntry{"realdebrid": {Provider: "realdebrid"}}
			e.SetActive("realdebrid", true)
		}
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseFailed,
		grab.PhaseDeleting:
	}
	st.Key = hash
	if st.Phase == grab.PhaseFailed {
		st.Failure = grab.Failure{Reason: "refused", At: time.Now()}
	}
	if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Queue().Add(e); err != nil {
		t.Fatal(err)
	}
}

func listTorrents(t *testing.T, q *QBit, query string) []Torrent {
	t.Helper()
	w := httptest.NewRecorder()
	q.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/torrents/info"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got []Torrent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

func newInfoQBit(t *testing.T) (*QBit, *manager.Manager) {
	t.Helper()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	return New(mgr, store, zerolog.Nop()), mgr
}

func TestTorrentsInfoFilterCompleted(t *testing.T) {
	t.Parallel()
	q, mgr := newInfoQBit(t)
	addGrab(t, mgr, "done", grab.State{Phase: grab.PhaseComplete})
	addGrab(t, mgr, "busy", grab.State{Phase: grab.PhaseProvider})
	got := listTorrents(t, q, "?filter=completed")
	if len(got) != 1 || got[0].Hash != "done" {
		t.Fatalf("filter=completed listed %+v, want only done", got)
	}
}

func TestTorrentsInfoStatesMatchLegacy(t *testing.T) {
	t.Parallel()
	q, mgr := newInfoQBit(t)
	want := []struct {
		phase grab.Phase
		state string
	}{
		{grab.PhaseQueued, "queuedDL"},
		{grab.PhaseThrottled, "queuedDL"},
		{grab.PhaseSubmitting, "queuedDL"},
		{grab.PhaseProvider, "downloading"},
		{grab.PhaseAwaitingLinks, "downloading"},
		{grab.PhaseActing, "downloading"},
		{grab.PhaseComplete, "pausedUP"},
		{grab.PhaseFailed, "error"},
	}
	for _, w := range want {
		addGrab(t, mgr, w.phase.String(), grab.State{Phase: w.phase})
	}
	got := listTorrents(t, q, "")
	if len(got) != len(want) {
		t.Fatalf("listed %d torrents, want %d: %+v", len(got), len(want), got)
	}
	for _, tor := range got {
		for _, w := range want {
			if w.phase.String() == tor.Hash && string(tor.State) != w.state {
				t.Errorf("%v lists as %q, want %q", w.phase, tor.State, w.state)
			}
		}
	}
}

func TestTorrentsInfoProjectionChanges(t *testing.T) {
	t.Parallel()
	q, mgr := newInfoQBit(t)
	addGrab(t, mgr, "done", grab.State{Phase: grab.PhaseComplete})
	addGrab(t, mgr, "child", grab.State{Phase: grab.PhaseActing, PackKey: "pack"})
	stored, err := mgr.Queue().GetTorrent("done")
	if err != nil || stored.CompletedAt == nil {
		t.Fatalf("stored complete grab: %v, %+v", err, stored)
	}
	for _, tor := range listTorrents(t, q, "") {
		switch tor.Hash {
		case "done":
			if tor.CompletionOn != stored.CompletedAt.Unix() {
				t.Errorf("completion_on = %d, want %d", tor.CompletionOn, stored.CompletedAt.Unix())
			}
		case "child":
			if tor.State != "downloading" || tor.Progress != 1 {
				t.Errorf("season child in acting lists %q at %v, want downloading at 1", tor.State, tor.Progress)
			}
		}
	}
}

func TestDeletingGrabIsNotListed(t *testing.T) {
	t.Parallel()
	q, mgr := newInfoQBit(t)
	addGrab(t, mgr, "going", grab.State{Phase: grab.PhaseDeleting})
	addGrab(t, mgr, "kept", grab.State{Phase: grab.PhaseProvider})
	got := listTorrents(t, q, "")
	if len(got) != 1 || got[0].Hash != "kept" {
		t.Fatalf("listed %+v, want only kept", got)
	}
}
