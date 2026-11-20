package sabnzbd

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
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
	if st.Phase == grab.PhaseFailed && st.Failure.Reason == "" {
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

func TestSabFailMessage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ cause, want string }{
		{grab.CauseArticlesMissing, "Download failed - Not on your server(s) - r"},
		{grab.CauseSegmentsMissing, "Aborted, cannot be completed - r"},
		{"", "r"},
		{"magnet_error", "r"},
	} {
		if got := sabFailMessage(c.cause, "r"); got != c.want {
			t.Errorf("sabFailMessage(%q) = %q, want %q", c.cause, got, c.want)
		}
	}
}

func TestSabFailMessagePrefixesMatchBindery(t *testing.T) {
	t.Parallel()
	prefixes := []string{
		"aborted, cannot be completed",
		"download failed - not on your server(s)",
		"download might fail, only",
		"repair failed, not enough repair blocks",
		"repairing failed, repair failed.",
		"unpacking failed, crc error",
		"unpacking failed, unable to find",
		"unpacking failed, archive requires a password",
		"corrupt rar file",
		"unusable rar file",
		"rar files failed to verify",
		"some files failed to verify against",
		"aborted, encryption detected",
		"aborted, unwanted extension detected",
	}
	matches := func(message string) bool {
		lowered := strings.ToLower(strings.TrimSpace(message))
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(lowered, p) })
	}
	raw := "failed to process nzb: file segment numbers are not contiguous"
	for _, cause := range []string{grab.CauseArticlesMissing, grab.CauseSegmentsMissing} {
		got := sabFailMessage(cause, raw)
		if !matches(got) {
			t.Errorf("%q: message %q has no Bindery content-failure prefix", cause, got)
		}
		if strings.EqualFold(got, "Unpacking failed, write error or disk is full?") {
			t.Errorf("%q: message equals the disk-full wording", cause)
		}
	}
	if matches(sabFailMessage("", raw)) {
		t.Errorf("an unclassified failure matches a Bindery prefix")
	}
}

func TestHistoryFailMessage(t *testing.T) {
	t.Parallel()
	_, router, mgr := newContractRouter(t)
	failedWith := func(cause string) grab.State {
		return grab.State{Phase: grab.PhaseFailed, Failure: grab.Failure{Reason: "r", Cause: cause, At: time.Now()}}
	}
	addNZBGrab(t, mgr, "dead", failedWith(grab.CauseArticlesMissing))
	addNZBGrab(t, mgr, "legacy", failedWith(""))
	addNZBGrab(t, mgr, "magnet", failedWith("magnet_error"))
	done := grab.State{Phase: grab.PhaseComplete, Failure: grab.Failure{Reason: "r", Cause: grab.CauseSegmentsMissing}}
	addNZBGrab(t, mgr, "done", done)

	var got struct {
		History struct {
			Slots []map[string]any `json:"slots"`
		} `json:"history"`
	}
	if err := json.Unmarshal(sabGet(t, router, url.Values{"mode": {"history"}, "category": {"tv"}}), &got); err != nil {
		t.Fatal(err)
	}
	messages := map[string]any{}
	for _, slot := range got.History.Slots {
		messages[slot["nzo_id"].(string)] = slot["fail_message"]
	}
	want := map[string]any{
		"dead": "Download failed - Not on your server(s) - r", "legacy": "r", "magnet": "r", "done": "r",
	}
	if !maps.Equal(messages, want) {
		t.Fatalf("fail_message by slot = %v, want %v", messages, want)
	}
}
