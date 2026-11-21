package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	polled chan string
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
	select {
	case p.polled <- t.ID:
	default:
	}
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
		cfg.Debrids = []config.Debrid{{Name: "realdebrid", Provider: "realdebrid", APIKey: "test"}}
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
	provider := &linksProvider{calls: map[string]int{}, answer: "t-answered", polled: make(chan string, 16)}
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
		if jsonErr := json.Unmarshal(w.Body.Bytes(), &got); jsonErr != nil {
			t.Fatal(jsonErr)
		}
		out := map[string]map[string]any{}
		for _, item := range got.Torrents {
			d, _ := item["deadline"].(map[string]any)
			out[item["info_hash"].(string)] = d
		}
		return out
	}
	seen := map[string]int{}
	for seen["t-held"] < 1 || seen["t-answered"] < 2 {
		select {
		case id := <-provider.polled:
			seen[id]++
		case <-t.Context().Done():
			t.Fatalf("polls %v never reached both grabs", seen)
		}
	}
	got := deadlines()
	kind := grab.EvLinkWaitExpired.String()
	if d := got["held"]; d["waiting_for"] != "links" || d["event"] != kind || !dueSet(d) {
		t.Fatalf("held deadline %v, want waiting for links", d)
	}
	if d := got["answered"]; d["event"] != kind || d["waiting_for"] != nil || !dueSet(d) {
		t.Fatalf("answered deadline %v, want the links limit without waiting_for", d)
	}
	if d, listed := got["done"]; !listed || d != nil {
		t.Fatalf("done deadline %v (%v), want none", d, got)
	}
}

func dueSet(d map[string]any) bool {
	raw, _ := d["due"].(string)
	due, err := time.Parse(time.RFC3339, raw)
	return err == nil && !due.IsZero()
}
