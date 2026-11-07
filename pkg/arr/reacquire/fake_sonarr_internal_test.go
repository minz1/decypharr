package reacquire

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

const (
	fakeSonarrFileID   = 5
	fakeSonarrSeriesID = 3
	fakeSonarrPath     = "/library/show/s01e01.mkv"
)

// fakeSonarr is the slice of Sonarr a reacquisition touches: one episode file
// covering the episodes in monitored, the delete, the search command and the
// monitor toggle.
type fakeSonarr struct {
	t      *testing.T
	server *httptest.Server

	mu          sync.Mutex
	filePresent bool
	monitored   map[int]bool
	// deleteMode decides what the next DELETE does: "ok" removes the file and
	// answers, "drop-landed" removes the file and drops the connection,
	// "drop-lost" drops the connection without removing the file, "refuse"
	// answers 403.
	deleteMode string
	// unmonitorOnDelete mimics Sonarr's "unmonitor deleted episodes" setting.
	unmonitorOnDelete bool
	deletes           int
	searches          int
	// blocklists counts POSTs of history/failed, which blocklist the grab.
	blocklists int
	// failMonitor answers the monitor toggle with a refusal.
	failMonitor bool
}

func newFakeSonarr(t *testing.T, episodeIDs ...int) *fakeSonarr {
	t.Helper()
	fake := &fakeSonarr{
		t:                 t,
		filePresent:       true,
		monitored:         make(map[int]bool, len(episodeIDs)),
		deleteMode:        "ok",
		unmonitorOnDelete: true,
	}
	for _, id := range episodeIDs {
		fake.monitored[id] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/config/downloadclient", fake.downloadClient)
	mux.HandleFunc(fmt.Sprintf("GET /api/v3/episodefile/%d", fakeSonarrFileID), fake.episodeFile)
	mux.HandleFunc("GET /api/v3/episode", fake.episodes)
	mux.HandleFunc(fmt.Sprintf("DELETE /api/v3/episodefile/%d", fakeSonarrFileID), fake.deleteFile)
	mux.HandleFunc("PUT /api/v3/episode/monitor", fake.monitor)
	mux.HandleFunc("POST /api/v3/command", fake.command)
	mux.HandleFunc("GET /api/v3/history", fake.history)
	mux.HandleFunc("POST /api/v3/history/failed/{id}", fake.blocklist)
	mux.HandleFunc("GET /api/v3/command", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `[]`) })
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeSonarr) instance() arr.Arr {
	return arr.Arr{Name: "sonarr", Host: fake.server.URL, Token: "secret", Type: arr.Sonarr}
}

func (fake *fakeSonarr) deleteCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.deletes
}

func (fake *fakeSonarr) searchCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.searches
}

func (fake *fakeSonarr) setDeleteMode(mode string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.deleteMode = mode
}

func (fake *fakeSonarr) downloadClient(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, `{"enableCompletedDownloadHandling":true}`)
}

func (fake *fakeSonarr) episodeFile(w http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.filePresent {
		http.NotFound(w, request)
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":%d,"seriesId":%d,"seasonNumber":1,"path":%q,"size":10}`,
		fakeSonarrFileID, fakeSonarrSeriesID, fakeSonarrPath)
}

func (fake *fakeSonarr) episodes(w http.ResponseWriter, _ *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	episodes := make([]map[string]any, 0, len(fake.monitored))
	for id, monitored := range fake.monitored {
		fileID := 0
		if fake.filePresent {
			fileID = fakeSonarrFileID
		}
		episodes = append(episodes, map[string]any{"id": id, "episodeFileId": fileID, "monitored": monitored})
	}
	_ = json.NewEncoder(w).Encode(episodes)
}

func (fake *fakeSonarr) deleteFile(w http.ResponseWriter, _ *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.deletes++
	switch fake.deleteMode {
	case "ok":
		fake.removeFile()
		w.WriteHeader(http.StatusOK)
	case "drop-landed":
		fake.removeFile()
		dropConnection(w)
	case "drop-lost":
		dropConnection(w)
	case "refuse":
		w.WriteHeader(http.StatusForbidden)
	default:
		fake.t.Errorf("unknown delete mode %q", fake.deleteMode)
	}
}

func (fake *fakeSonarr) isMonitored(id int) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.monitored[id]
}

// loseFile removes the file as a DELETE that landed would, without a request.
func (fake *fakeSonarr) loseFile() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.removeFile()
}

func (fake *fakeSonarr) setMonitorFailing(failing bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.failMonitor = failing
}

func (fake *fakeSonarr) monitor(w http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.failMonitor {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var body struct {
		EpisodeIDs []int `json:"episodeIds"`
		Monitored  bool  `json:"monitored"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		fake.t.Errorf("decode monitor request: %v", err)
	}
	for _, id := range body.EpisodeIDs {
		fake.monitored[id] = body.Monitored
	}
	w.WriteHeader(http.StatusAccepted)
}

func (fake *fakeSonarr) blocklistCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.blocklists
}

// history holds the grab of the download, and its failure once blocklisted.
func (fake *fakeSonarr) history(w http.ResponseWriter, _ *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	records := []map[string]any{{"id": 7, "downloadId": "download", "eventType": "grabbed"}}
	if fake.blocklists > 0 {
		records = append(records, map[string]any{"id": 8, "downloadId": "download", "eventType": "downloadFailed"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"page": 1, "totalRecords": len(records), "records": records})
}

func (fake *fakeSonarr) blocklist(w http.ResponseWriter, _ *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.blocklists++
	w.WriteHeader(http.StatusOK)
}

func (fake *fakeSonarr) command(w http.ResponseWriter, _ *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.searches++
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"id":%d,"name":"EpisodeSearch","status":"queued"}`, 700+fake.searches)
}

func (fake *fakeSonarr) removeFile() {
	fake.filePresent = false
	if fake.unmonitorOnDelete {
		for id := range fake.monitored {
			fake.monitored[id] = false
		}
	}
}

func dropConnection(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// fakeSonarrBinding is the binding of the fake's one file.
func fakeSonarrBinding(instance arr.Arr, episodeIDs ...int) Binding {
	return Binding{
		ArrName:                instance.Name,
		ArrType:                arr.Sonarr,
		ArrInstanceFingerprint: instance.Fingerprint(),
		EntryID:                "entry",
		EntryFileID:            "file",
		DownloadID:             "download",
		ArrFileID:              fakeSonarrFileID,
		LibraryPath:            fakeSonarrPath,
		SeriesID:               fakeSonarrSeriesID,
		SeasonNumber:           1,
		EpisodeIDs:             episodeIDs,
		Confidence:             ConfidenceExactPath,
	}
}
