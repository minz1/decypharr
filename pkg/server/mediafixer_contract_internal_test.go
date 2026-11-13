package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/sessions"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const mediafixerToken = "mediafixer-token"

func mediafixerFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "mediafixer", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeInto[T any](t *testing.T, data []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func reencode(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return decodeInto[map[string]any](t, data)
}

func assertContractFields(t *testing.T, what string, want, got map[string]any, fields ...string) {
	t.Helper()
	for _, field := range fields {
		wantValue, ok := want[field]
		if !ok {
			t.Fatalf("%s: the fixture has no %q", what, field)
		}
		gotValue, ok := got[field]
		if !ok {
			t.Errorf("%s: field %q is gone", what, field)
			continue
		}
		if !reflect.DeepEqual(wantValue, gotValue) && !sameInstant(wantValue, gotValue) {
			t.Errorf("%s: %q = %v, want %v", what, field, gotValue, wantValue)
		}
	}
}

func sameInstant(want, got any) bool {
	wantText, wantOK := want.(string)
	gotText, gotOK := got.(string)
	if !wantOK || !gotOK {
		return false
	}
	wantTime, wantErr := time.Parse(time.RFC3339, wantText)
	gotTime, gotErr := time.Parse(time.RFC3339, gotText)
	return wantErr == nil && gotErr == nil && wantTime.Equal(gotTime)
}

func mediafixerStore(t *testing.T) (*manager.Manager, *config.Store) {
	t.Helper()
	downloads := t.TempDir()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.DownloadFolder = downloads
		cfg.Debrids = []config.Debrid{{Name: "torbox", APIKey: "key"}}
	})
	if err := store.Get().SaveAuth(&config.Auth{APIToken: mediafixerToken, TokenOnly: true}); err != nil {
		t.Fatal(err)
	}
	return mgr, store
}

func TestMediafixerContractRoutes(t *testing.T) {
	t.Parallel()
	_, store := mediafixerStore(t)
	server := newTestServer(t, store)
	server.cookie = sessions.NewCookieStore([]byte("test-secret"))
	routes, ok := server.WebRoutes().(chi.Routes)
	if !ok {
		t.Fatal("WebRoutes is not a chi router")
	}
	registered := make(map[string]bool)
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{
		"GET /api/torrents",
		"POST /api/repair/run",
		"GET /api/repair/status",
		"GET /api/repair/health",
		"POST /api/repair/health/{name}/check",
		"POST /api/mount/cache/cleanup",
	} {
		if !registered[route] {
			t.Errorf("route %s is gone", route)
		}
	}
}

func TestMediafixerContractTorrentsByHash(t *testing.T) {
	t.Parallel()
	fixture := decodeInto[map[string]any](t, mediafixerFixture(t, "torrents.json"))
	wantEntry, ok := fixture["torrents"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatal("torrents.json has no entry")
	}
	listing := decodeInto[struct {
		Torrents []*storage.Entry `json:"torrents"`
	}](t, mediafixerFixture(t, "torrents.json"))
	entry := listing.Torrents[0]

	mgr, store := mediafixerStore(t)
	if err := mgr.Queue().Add(entry); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, store)
	server.manager = mgr
	server.cookie = sessions.NewCookieStore([]byte("test-secret"))

	request := httptest.NewRequest(http.MethodGet,
		"/api/torrents?search="+entry.InfoHash+"&state=&page=1&limit=100", nil)
	request.Header.Set("Authorization", "Bearer "+mediafixerToken)
	response := httptest.NewRecorder()
	server.WebRoutes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	body := decodeInto[map[string]any](t, response.Body.Bytes())
	if body["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", body["total"])
	}
	torrents, ok := body["torrents"].([]any)
	if !ok || len(torrents) != 1 {
		t.Fatalf("torrents = %v", body["torrents"])
	}
	gotEntry, ok := torrents[0].(map[string]any)
	if !ok {
		t.Fatalf("torrent = %v", torrents[0])
	}
	assertContractFields(t, "GET /api/torrents", wantEntry, gotEntry,
		"name", "info_hash", "category", "state", "size", "progress", "added_on", "bad", "active_provider",
		"original_filename")
}

func TestMediafixerContractFolderLayout(t *testing.T) {
	t.Parallel()
	listing := decodeInto[struct {
		Torrents []struct {
			storage.Entry

			MountPath   string `json:"mount_path"`
			ContentPath string `json:"content_path"`
		} `json:"torrents"`
	}](t, mediafixerFixture(t, "torrents.json"))
	entry := listing.Torrents[0]
	folder := entry.GetFolder(config.WebDavUseOriginalNameNoExt)
	if folder != path.Base(entry.MountPath) || folder != path.Base(entry.ContentPath) {
		t.Fatalf("folder %q, mount path %q, content path %q", folder, entry.MountPath, entry.ContentPath)
	}
	if path.Base(path.Dir(entry.MountPath)) != manager.EntryAllFolder {
		t.Fatalf("mount path %q is not under %s", entry.MountPath, manager.EntryAllFolder)
	}
}

func TestMediafixerContractRepairRun(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"auto_repair": true, "unrestrict_link": true}`, `{"auto_repair": true}`} {
		decoder := json.NewDecoder(bytes.NewBufferString(body))
		decoder.DisallowUnknownFields()
		var request repairRunRequest
		if err := decoder.Decode(&request); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if request.AutoRepair == nil || !*request.AutoRepair {
			t.Fatalf("%s: auto_repair not read", body)
		}
		if want := strings.Contains(body, "unrestrict_link"); request.UnrestrictLink != want {
			t.Fatalf("%s: unrestrict_link = %v", body, request.UnrestrictLink)
		}
	}
	fixture := decodeInto[map[string]any](t, mediafixerFixture(t, "repair_run.json"))
	response := decodeInto[repairRunResponse](t, mediafixerFixture(t, "repair_run.json"))
	assertContractFields(t, "POST /api/repair/run", fixture, reencode(t, response), "run_id")
}

func TestMediafixerContractRepairStatus(t *testing.T) {
	t.Parallel()
	fixture := decodeInto[map[string]any](t, mediafixerFixture(t, "repair_status_running.json"))
	status := decodeInto[repair.Status](t, mediafixerFixture(t, "repair_status_running.json"))
	got := reencode(t, status)
	activeRun, ok := got["active_run"].(map[string]any)
	if !ok {
		t.Fatalf("active_run = %v", got["active_run"])
	}
	wantRun, ok := fixture["active_run"].(map[string]any)
	if !ok {
		t.Fatal("repair_status_running.json has no active_run")
	}
	assertContractFields(t, "GET /api/repair/status active_run", wantRun, activeRun, "stage")
	if idle := reencode(t, repair.Status{}); idle["active_run"] != nil {
		t.Fatalf("an idle status reads as running: active_run = %v", idle["active_run"])
	}
}

func TestMediafixerContractRepairHealth(t *testing.T) {
	t.Parallel()
	fixture := decodeInto[[]map[string]any](t, mediafixerFixture(t, "repair_health.json"))
	health := decodeInto[[]*storage.EntryHealth](t, mediafixerFixture(t, "repair_health.json"))
	if len(health) != len(fixture) || len(health) == 0 {
		t.Fatalf("decoded %d entries from %d", len(health), len(fixture))
	}
	for i, entry := range health {
		assertContractFields(t, "GET /api/repair/health", fixture[i], reencode(t, entry), "entry_name", "status")
	}
}
