package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type linksProvider struct {
	debrid.Client

	mu     sync.Mutex
	calls  map[string]int
	answer string
}

func (*linksProvider) Config() config.Debrid { return config.Debrid{Name: "realdebrid"} }

func (p *linksProvider) Poll(
	ctx context.Context,
	t *debridTypes.Torrent,
	_ bool,
) (*debridTypes.Torrent, debridTypes.Observation, error) {
	p.mu.Lock()
	p.calls[t.ID]++
	first := p.calls[t.ID] == 1
	p.mu.Unlock()
	if t.ID == p.answer && first {
		return t, debridTypes.Observation{Phase: debridTypes.PhaseFinalizing, LinksPending: true}, nil
	}
	<-ctx.Done()
	return nil, debridTypes.Observation{}, ctx.Err()
}

func seedGrab(t *testing.T, db *storage.Storage, key string, st grab.State, downloaded time.Duration) {
	t.Helper()
	at := time.Now().Add(-downloaded)
	e := &storage.Entry{
		InfoHash: key, Name: key, Category: "sonarr", Protocol: config.ProtocolTorrent, Size: 100, Progress: 1,
		AddedOn: time.Now().Add(-downloaded),
		Providers: map[string]*storage.ProviderEntry{
			"realdebrid": {Provider: "realdebrid", ID: "t-" + key, DownloadedAt: &at},
		},
	}
	e.SetActive("realdebrid", true)
	st.Key, st.PhaseSince = key, time.Now()
	if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateQueuedGrab(e); err != nil {
		t.Fatal(err)
	}
}

func TestGetTorrentsShowsTheDeadlinesTheEntryActorsDerive(t *testing.T) {
	t.Parallel()
	store := managertest.Store(t, func(cfg *config.Config) {
		cfg.UseAuth = false
		cfg.RefreshInterval = "20ms"
	})
	db, err := storage.NewStorage(filepath.Join(store.Get().Dir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedGrab(t, db, "held", grab.State{Phase: grab.PhaseAwaitingLinks}, 7*time.Hour)
	seedGrab(t, db, "answered", grab.State{Phase: grab.PhaseAwaitingLinks}, time.Hour)
	seedGrab(t, db, "done", grab.State{Phase: grab.PhaseComplete}, time.Hour)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &linksProvider{calls: map[string]int{}, answer: "t-answered"}
	mgr := managertest.NewOn(t, store, manager.WithClients(map[string]debrid.Client{"realdebrid": provider}))
	s := &Server{manager: mgr, config: store}

	deadlines := func() map[string]map[string]any {
		w := httptest.NewRecorder()
		s.handleGetTorrents(w, httptest.NewRequest(http.MethodGet, "/api/torrents", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var got struct {
			Torrents []map[string]any `json:"torrents"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, item := range got.Torrents {
			d, _ := item["deadline"].(map[string]any)
			out[item["info_hash"].(string)] = d
		}
		return out
	}
	ready := func(got map[string]map[string]any) bool {
		return got["held"] != nil && got["answered"] != nil && got["answered"]["waiting_for"] == nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got := deadlines()
	for ctx.Err() == nil && !ready(got) {
		runtime.Gosched()
		got = deadlines()
	}
	if !ready(got) {
		t.Fatalf("deadlines %v never settled", got)
	}
	kind := grab.EvLinkWaitExpired.String()
	if d := got["held"]; d["waiting_for"] != "links" || d["event"] != kind || d["due"] == "" {
		t.Fatalf("held deadline %v, want waiting for links", d)
	}
	if d := got["answered"]; d["event"] != kind || d["due"] == "" {
		t.Fatalf("answered deadline %v, want the links limit without waiting_for", d)
	}
	if d, listed := got["done"]; !listed || d != nil {
		t.Fatalf("done deadline %v (%v), want none", d, got)
	}
}
