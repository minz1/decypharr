package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func queueGrabAt(t *testing.T, mgr *manager.Manager, key string, st grab.State) {
	t.Helper()
	e := &storage.Entry{
		InfoHash: key, Name: key, Category: "sonarr", Protocol: config.ProtocolTorrent, Size: 100, Progress: 0.4,
	}
	switch st.Phase {
	case grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseComplete:
		e.Providers = map[string]*storage.ProviderEntry{"realdebrid": {Provider: "realdebrid"}}
		e.SetActive("realdebrid", true)
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseFailed,
		grab.PhaseDeleting:
	}
	st.Key = key
	switch st.Phase {
	case grab.PhaseFailed:
		st.Failure = grab.Failure{Reason: "refused", At: time.Now()}
	case grab.PhaseDeleting:
		st.Cleanup = grab.Cleanup{DeleteFiles: true}
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseProvider,
		grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseComplete:
	}
	if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Queue().Add(e); err != nil {
		t.Fatal(err)
	}
}

type dashboardItem struct {
	InfoHash string `json:"info_hash"`
	Phase    string `json:"phase"`
	Label    string `json:"label"`
}

func listDashboard(t *testing.T, s *Server, query string) []dashboardItem {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleGetTorrents(w, httptest.NewRequest(http.MethodGet, "/api/torrents"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Torrents []dashboardItem `json:"torrents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Torrents
}

func TestTorrentsAPICarriesPhase(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	s := &Server{manager: mgr, config: store}
	queueGrabAt(t, mgr, "busy", grab.State{Phase: grab.PhaseProvider})
	queueGrabAt(t, mgr, "done", grab.State{Phase: grab.PhaseComplete})
	queueGrabAt(t, mgr, "wait", grab.State{Phase: grab.PhaseThrottled})

	want := map[string][2]string{
		"busy": {"provider", "Downloading"}, "done": {"complete", "Completed"}, "wait": {"throttled", "Waiting"},
	}
	all := listDashboard(t, s, "")
	if len(all) != len(want) {
		t.Fatalf("listed %+v, want %d items", all, len(want))
	}
	for _, item := range all {
		if got := [2]string{item.Phase, item.Label}; got != want[item.InfoHash] {
			t.Errorf("%s lists %v, want %v", item.InfoHash, got, want[item.InfoHash])
		}
	}
	complete := listDashboard(t, s, "?state=complete")
	if len(complete) != 1 || complete[0].InfoHash != "done" {
		t.Fatalf("state=complete listed %+v, want only done", complete)
	}
}

func TestDeletingGrabOnDashboard(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	s := &Server{manager: mgr, config: store}
	queueGrabAt(t, mgr, "going", grab.State{Phase: grab.PhaseDeleting})
	got := listDashboard(t, s, "")
	if len(got) != 1 || got[0].Phase != "deleting" || got[0].Label != "Deleting" {
		t.Fatalf("listed %+v, want the deleting grab with its label", got)
	}
}
