package sabnzbd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func addNZBGrab(t *testing.T, mgr *manager.Manager, hash string, st grab.State) {
	t.Helper()
	e := &storage.Entry{
		InfoHash: hash, Name: hash + ".nzb", Category: "tv", Protocol: config.ProtocolNZB, Size: 4 << 20,
		Progress: 0.25, SavePath: t.TempDir(),
		Files: map[string]*storage.File{"video.mkv": {Name: "video.mkv", Size: 1 << 20}},
	}
	st.Key = hash
	st.NZB = true
	if st.Phase == grab.PhaseFailed {
		st.Failure = grab.Failure{Reason: "missing segments", At: time.Now()}
	}
	if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Queue().Add(e); err != nil {
		t.Fatal(err)
	}
}

func sabGet(t *testing.T, router http.Handler, values url.Values) []byte {
	t.Helper()
	values.Set("ma_password", "test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, sabRequest(http.MethodGet, values))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

func queueNzoIDs(t *testing.T, router http.Handler, mode string) []string {
	t.Helper()
	body := sabGet(t, router, url.Values{"mode": {mode}, "category": {"tv"}})
	var ids []string
	switch mode {
	case "queue":
		var got QueueResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		for _, s := range got.Queue.Slots {
			ids = append(ids, s.NzoID)
		}
	default:
		var got HistoryResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		for _, s := range got.History.Slots {
			ids = append(ids, s.NzoID)
		}
	}
	return ids
}

func TestDeletingGrabIsNotListed(t *testing.T) {
	t.Parallel()
	_, router, mgr := newContractRouter(t)
	addNZBGrab(t, mgr, "going", grab.State{Phase: grab.PhaseDeleting})
	addNZBGrab(t, mgr, "kept", grab.State{Phase: grab.PhaseProvider})
	for _, mode := range []string{"queue", "history"} {
		for _, id := range queueNzoIDs(t, router, mode) {
			if id == "going" {
				t.Errorf("mode=%s lists the deleting grab", mode)
			}
		}
	}
	if ids := queueNzoIDs(t, router, "queue"); len(ids) != 1 || ids[0] != "kept" {
		t.Errorf("queue lists %v, want only kept", ids)
	}
}

func TestSABHistoryListsFailed(t *testing.T) {
	t.Parallel()
	_, router, mgr := newContractRouter(t)
	addNZBGrab(t, mgr, "bad", grab.State{Phase: grab.PhaseFailed})
	addNZBGrab(t, mgr, "good", grab.State{Phase: grab.PhaseComplete})
	addNZBGrab(t, mgr, "busy", grab.State{Phase: grab.PhaseProvider})

	var got HistoryResponse
	if err := json.Unmarshal(sabGet(t, router, url.Values{"mode": {"history"}, "category": {"tv"}}), &got); err != nil {
		t.Fatal(err)
	}
	status := map[string]HistorySlot{}
	for _, s := range got.History.Slots {
		status[s.NzoID] = s
	}
	if len(status) != 2 || status["bad"].Status != StatusFailed || status["bad"].FailMessage != "missing segments" ||
		status["good"].Status != StatusCompleted {
		t.Fatalf("history = %+v", got.History.Slots)
	}
	if ids := queueNzoIDs(t, router, "queue"); len(ids) != 1 || ids[0] != "busy" {
		t.Fatalf("queue lists %v, want only busy", ids)
	}
}

func TestGetFilesFailedStatus(t *testing.T) {
	t.Parallel()
	_, router, mgr := newContractRouter(t)
	addNZBGrab(t, mgr, "bad", grab.State{Phase: grab.PhaseFailed})
	var got struct {
		Files []File `json:"files"`
	}
	body := sabGet(t, router, url.Values{"mode": {"get_files"}, "value": {"bad"}})
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Status != "failed" {
		t.Fatalf("files = %+v, want one failed file", got.Files)
	}
}

func TestGetFilesHidesDeletingGrab(t *testing.T) {
	t.Parallel()
	_, router, mgr := newContractRouter(t)
	addNZBGrab(t, mgr, "going", grab.State{Phase: grab.PhaseDeleting})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, sabRequest(http.MethodGet,
		url.Values{"mode": {"get_files"}, "value": {"going"}, "ma_password": {"test-token"}}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get_files of a deleting grab = %d %s, want 404", rec.Code, rec.Body)
	}
}

func queueGrab(t *testing.T, mgr *manager.Manager, e *storage.Entry, phase grab.Phase) {
	t.Helper()
	st := grab.State{Key: e.InfoHash, Phase: phase, NZB: e.Protocol == config.ProtocolNZB}
	if phase == grab.PhaseFailed {
		st.Failure = grab.Failure{Reason: "missing segments", At: time.Now()}
	}
	if phase == grab.PhaseProvider && !st.NZB {
		e.Providers = map[string]*storage.ProviderEntry{"realdebrid": {Provider: "realdebrid"}}
		e.SetActive("realdebrid", true)
	}
	if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Queue().Add(e); err != nil {
		t.Fatal(err)
	}
}
