package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func TestMergeConfigUpdatePreservesOmittedFields(t *testing.T) {
	t.Parallel()
	current := config.Config{
		Port:     "9000",
		LogLevel: "info",
		Debrids: []config.Debrid{{
			Name:   "realdebrid",
			APIKey: "secret",
		}},
		Mount: config.Mount{
			Type:      config.MountTypeDFS,
			MountPath: "/mnt/decypharr",
		},
		Notifications: config.Notifications{
			Enabled:    true,
			WebhookURL: "https://example.com/webhook",
		},
	}

	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"log_level":"debug"}`))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	if merged.LogLevel != "debug" {
		t.Fatalf("expected updated log level, got %q", merged.LogLevel)
	}
	if merged.Port != current.Port {
		t.Fatalf("expected port %q to be preserved, got %q", current.Port, merged.Port)
	}
	if !reflect.DeepEqual(merged.Debrids, current.Debrids) {
		t.Fatalf("expected debrid config to be preserved, got %#v", merged.Debrids)
	}
	if !reflect.DeepEqual(merged.Mount, current.Mount) {
		t.Fatalf("expected mount config to be preserved, got %#v", merged.Mount)
	}
	if !reflect.DeepEqual(merged.Notifications, current.Notifications) {
		t.Fatalf("expected notification config to be preserved, got %#v", merged.Notifications)
	}
}

func TestMergeConfigUpdateMergesNestedObjects(t *testing.T) {
	t.Parallel()
	current := config.Config{
		Mount: config.Mount{
			Type:      config.MountTypeRclone,
			MountPath: "/mnt/decypharr",
		},
	}

	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"mount":{"type":"dfs"}}`))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	if merged.Mount.Type != config.MountTypeDFS {
		t.Fatalf("expected mount type %q, got %q", config.MountTypeDFS, merged.Mount.Type)
	}
	if merged.Mount.MountPath != current.Mount.MountPath {
		t.Fatalf("expected mount path %q to be preserved, got %q", current.Mount.MountPath, merged.Mount.MountPath)
	}
}

func TestMergeConfigUpdateAllowsExplicitClear(t *testing.T) {
	t.Parallel()
	current := config.Config{Debrids: []config.Debrid{{Name: "realdebrid", APIKey: "secret"}}}

	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"debrids":[]}`))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	if len(merged.Debrids) != 0 {
		t.Fatalf("expected debrid config to be cleared, got %#v", merged.Debrids)
	}
}

func TestConfigHandlersUseSnapshots(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	before := store.Get()
	mgr.Arr().
		AddOrUpdate(arr.Arr{Name: "manual", Host: "http://example.test", Token: "token", Source: arr.SourceManual})
	server := &Server{manager: mgr, config: store}
	response := httptest.NewRecorder()
	server.handleGetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d", response.Code)
	}
	if len(before.Arrs) != 0 {
		t.Fatal("GET changed the current snapshot")
	}
	response = httptest.NewRecorder()
	server.handleUpdateConfig(
		response,
		httptest.NewRequest(
			http.MethodPost,
			"/api/config",
			strings.NewReader(`{"app_url":"https://new.example.test"}`),
		),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	if before.AppURL == "https://new.example.test" {
		t.Fatal("POST changed the previous snapshot")
	}
	if store.Get().AppURL != "https://new.example.test" {
		t.Fatal("POST did not publish the update")
	}
	var result struct {
		Restarted bool `json:"restarted"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Restarted {
		t.Fatal("live URL update restarted services")
	}
}

// List items merge by name, not by index: an item keeps its own omitted
// fields, never those of the item that used to sit at its index. Removing
// the first debrid used to hand its API key to the one that moved up.
func TestMergeConfigUpdateMatchesListItemsByName(t *testing.T) {
	t.Parallel()
	current := config.Config{
		Debrids: []config.Debrid{
			{Name: "realdebrid", APIKey: "rd-key", Workers: 4},
			{Name: "torbox", APIKey: "tb-key", Workers: 8, DownloadAPIKeys: []string{"a", "b"}},
		},
		Mount: config.Mount{Type: config.MountTypeDFS, MountPath: "/mnt/decypharr"},
		Usenet: config.Usenet{Providers: []config.UsenetProvider{
			{Host: "news.a", Username: "a"},
			{Host: "news.b", Username: "b", TLSServerName: "b.example"},
		}},
	}

	body := `{"debrids":[{"name":"torbox","download_api_keys":["c"]},{"name":"alldebrid","api_key":"ad-key"}],` +
		`"mount":{"type":"rclone"},"usenet":{"providers":[{"host":"news.b","username":"b2"}]}}`
	merged, err := mergeConfigUpdate(&current, strings.NewReader(body))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}
	want := []config.Debrid{
		{Name: "torbox", APIKey: "tb-key", Workers: 8, DownloadAPIKeys: []string{"c"}},
		{Name: "alldebrid", APIKey: "ad-key"},
	}
	if !reflect.DeepEqual(merged.Debrids, want) {
		t.Fatalf("debrids = %+v, want %+v", merged.Debrids, want)
	}
	if merged.Mount.MountPath != current.Mount.MountPath || merged.Mount.Type != config.MountTypeRclone {
		t.Fatalf("mount = %#v, want the type updated and the path kept", merged.Mount)
	}
	// Providers have no name; they match by host. The UI does not send
	// tls_server_name, so it must survive a save.
	wantProviders := []config.UsenetProvider{{Host: "news.b", Username: "b2", TLSServerName: "b.example"}}
	if !reflect.DeepEqual(merged.Usenet.Providers, wantProviders) {
		t.Fatalf("providers = %+v, want %+v", merged.Usenet.Providers, wantProviders)
	}
	if current.Debrids[0].APIKey != "rd-key" ||
		!reflect.DeepEqual(current.Debrids[1].DownloadAPIKeys, []string{"a", "b"}) {
		t.Fatal("merge changed the current config")
	}
}

// A save that the next Load would refuse is rejected with a 400 and never
// written: saving it used to crash every restart until config.json was
// fixed by hand.
func TestUpdateConfigRejectsUnloadableSettings(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"bad shared mode":     `{"shared_dir_mode":"775x"}`,
		"missing tls ca file": `{"tls_ca_file":"/nonexistent/ca.pem"}`,
		"bad dfs size":        `{"mount":{"dfs":{"chunk_size":"lots"}}}`,
		"bad debrid proxy":    `{"debrids":[{"name":"rd","provider":"realdebrid","api_key":"k","proxy":"http://[::1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mgr, store := managertest.New(t, nil)
			if err := store.Get().Save(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.Get().JSONFile())
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
			response := httptest.NewRecorder()
			server.handleUpdateConfig(
				response,
				httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)),
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", response.Code, response.Body.String())
			}
			after, err := os.ReadFile(store.Get().JSONFile())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the rejected settings were written to config.json")
			}
			if _, loadErr := config.Load(store.Get().Dir(), config.MapEnv(nil)); loadErr != nil {
				t.Fatalf("config.json no longer loads: %v", loadErr)
			}
		})
	}
}
