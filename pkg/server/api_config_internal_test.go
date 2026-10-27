package server

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestUpdateConfigKeepsAuthAndSessionSecret(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.Auth = &config.Auth{Username: "owner", Password: "hash", APIToken: "token"}
	})
	before := store.Get()
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
	response := httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{
		"app_url": "https://new.example.test",
		"auth": {"username": "intruder", "password": "x", "api_token": "stolen"},
		"Auth": {"username": "intruder", "password": "x", "api_token": "stolen"},
		"session_secret": "attacker-secret",
		"SessionSecret": {"value": "attacker-secret"}
	}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	after := store.Get()
	if after.AppURL != "https://new.example.test" {
		t.Fatal("POST did not publish the update")
	}
	if after.Auth == nil || *after.Auth != *before.Auth {
		t.Fatalf("auth = %+v, want %+v", after.Auth, before.Auth)
	}
	if after.SessionSecret.IsZero() || after.SessionSecret != before.SessionSecret {
		t.Fatal("POST replaced the session secret")
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
		"bad shared mode":       `{"shared_dir_mode":"775x"}`,
		"missing tls ca file":   `{"tls_ca_file":"/nonexistent/ca.pem"}`,
		"bad dfs size":          `{"mount":{"dfs":{"chunk_size":"lots"}}}`,
		"bad debrid proxy":      `{"debrids":[{"name":"rd","provider":"realdebrid","api_key":"k","proxy":"http://[::1"}]}`,
		"bad hearsay blocklist": `{"hearsay":{"blocklist":["10.0.0.1"]}}`,
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

func TestUpdateConfigPruneWithStrmNamesStrm(t *testing.T) {
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
	body := `{"strm":{"enabled":true},"prune":{"enabled":true,"roots":["/data"]}}`
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "together with STRM") {
		t.Fatalf("status = %d (%s), want 400 naming STRM", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(store.Get().JSONFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the rejected settings were written to config.json")
	}
}

func TestUpdateConfigUntouchedPruneKeepsReport(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	if err := store.Get().Save(); err != nil {
		t.Fatal(err)
	}
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
	body := []byte(`{"prune":{"enabled":false,"mode":"report","schedule":"1h","grace":"168h",` +
		`"max_per_run":100,"roots":[]}}`)
	if postConfig(t, server, body) {
		t.Fatal("an untouched prune section restarted services")
	}
	if got := store.Get().Prune; got.Mode != config.PruneModeReport || got.Enabled || len(got.Roots) != 0 {
		t.Fatalf("prune = %#v, want the disabled report defaults", got)
	}
}

func TestMergeConfigUpdatePreservesPrune(t *testing.T) {
	t.Parallel()
	current := config.Config{Prune: config.Prune{
		Enabled: true, Mode: config.PruneModeDelete, Schedule: "1h", Grace: "168h",
		MaxPerRun: 50, Roots: []string{"/data/library"},
	}}
	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"log_level":"debug"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged.Prune, current.Prune) {
		t.Fatalf("prune not preserved: %#v", merged.Prune)
	}
}

func writeTestCA(t *testing.T) string {
	t.Helper()
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
	tlsServer.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return caFile
}

func postConfig(t *testing.T, server *Server, body []byte) bool {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", response.Code, response.Body.String())
	}
	var result struct {
		Restarted bool `json:"restarted"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Restarted
}

func readConfigJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err = json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func phase1Payload(t *testing.T, caFile string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"allow_samples":    true,
		"categories":       []string{"tv", "movies"},
		"retries":          5,
		"shared_dir_mode":  "2775",
		"shared_file_mode": "0664",
		"tls_ca_file":      caFile,
		"debrids": []any{map[string]any{
			"name": "rd", "provider": "realdebrid", "api_key": "k", "limit": 500,
		}},
		"arrs": []any{map[string]any{
			"name": "sonarr", "host": "http://sonarr:8989", "token": "t", "import_webhook": false,
		}},
		"usenet": map[string]any{
			"stream_backup_wait":  "2s",
			"socket_read_buffer":  "8MB",
			"socket_write_buffer": "0",
			"providers": []any{map[string]any{
				"host": "news.example", "port": 563, "username": "u", "password": "p",
				"tls_server_name": "news.reseller.example",
			}},
		},
		"mount": map[string]any{
			"dfs": map[string]any{
				"disable_cache":       true,
				"drop_behind_margin":  "256MB",
				"fuse_max_background": 32,
				"fuse_max_read_ahead": "512KB",
			},
			"rclone": map[string]any{"vfs_disk_space_total": "1G"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func phase1Checks(got *config.Config, caFile string) map[string][2]any {
	return map[string][2]any{
		"allow_samples":         {got.AllowSamples, true},
		"categories":            {got.Categories, []string{"tv", "movies"}},
		"retries":               {got.Retries, 5},
		"shared_dir_mode":       {got.SharedDirMode, "2775"},
		"shared_file_mode":      {got.SharedFileMode, "0664"},
		"tls_ca_file":           {got.TLSCAFile, caFile},
		"debrids[].limit":       {len(got.Debrids) == 1 && got.Debrids[0].Limit == 500, true},
		"arrs[].import_webhook": {len(got.Arrs) == 1 && !got.Arrs[0].ImportWebhook, true},
		"usenet.providers[].tls_server_name": {
			len(got.Usenet.Providers) == 1 && got.Usenet.Providers[0].TLSServerName == "news.reseller.example", true,
		},
		"usenet.stream_backup_wait":         {got.Usenet.StreamBackupWait, "2s"},
		"usenet.socket_read_buffer":         {got.Usenet.SocketReadBuffer, "8MB"},
		"usenet.socket_write_buffer":        {got.Usenet.SocketWriteBuffer, "0"},
		"mount.dfs.disable_cache":           {got.Mount.DFS.DisableCache, true},
		"mount.dfs.drop_behind_margin":      {got.Mount.DFS.DropBehindMargin, "256MB"},
		"mount.dfs.fuse_max_background":     {got.Mount.DFS.FuseMaxBackground, 32},
		"mount.dfs.fuse_max_read_ahead":     {got.Mount.DFS.FuseMaxReadAhead, "512KB"},
		"mount.rclone.vfs_disk_space_total": {got.Mount.Rclone.VfsDiskSpaceTotal, "1G"},
	}
}

func TestUpdateConfigSettingsPagePhase1Fields(t *testing.T) {
	t.Parallel()
	caFile := writeTestCA(t)
	mgr, store := managertest.New(t, func(c *config.Config) {
		c.Debrids = []config.Debrid{{Name: "rd", Provider: "realdebrid", APIKey: "k"}}
		c.Arrs = []config.Arr{{Name: "sonarr", Host: "http://sonarr:8989", Token: "t", ImportWebhook: true}}
		c.Usenet.Providers = []config.UsenetProvider{{Host: "news.example", Port: 563, Username: "u", Password: "p"}}
	})
	if err := store.Get().Save(); err != nil {
		t.Fatal(err)
	}
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
	saveWithoutEdits(t, server, store, "fresh config")

	postConfig(t, server, phase1Payload(t, caFile))
	got, err := config.Load(store.Get().Dir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	for path, c := range phase1Checks(got, caFile) {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("edit: %s = %v, want %v", path, c[0], c[1])
		}
	}
	saveWithoutEdits(t, server, store, "after the edit")
}

// saveWithoutEdits posts the settings the page loaded back unchanged and checks config.json kept every value.
func saveWithoutEdits(t *testing.T, server *Server, store *config.Store, step string) {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleGetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s: GET status = %d", step, response.Code)
	}
	before := readConfigJSON(t, store.Get().JSONFile())
	if postConfig(t, server, response.Body.Bytes()) {
		t.Errorf("%s: a save of the unchanged settings restarted services", step)
	}
	if after := readConfigJSON(t, store.Get().JSONFile()); !reflect.DeepEqual(before, after) {
		t.Errorf("%s: config.json changed on a save with no edits:\nbefore %v\nafter  %v", step, before, after)
	}
}

func TestConfigResponseCarriesArrsAndAuth(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		LogLevel: "debug",
		UseAuth:  true,
		Auth:     &config.Auth{Username: "admin", APIToken: "token", TokenOnly: true},
		Arrs:     []config.Arr{{Name: "stale"}},
	}
	body, err := json.Marshal(configResponse(cfg, []config.Arr{{Name: "sonarr", Host: "http://sonarr:8989"}}))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		LogLevel      string       `json:"log_level"`
		Arrs          []config.Arr `json:"arrs"`
		APIToken      string       `json:"api_token"`
		AuthUsername  string       `json:"auth_username"`
		AuthTokenOnly bool         `json:"auth_token_only"`
	}
	if unmarshalErr := json.Unmarshal(body, &got); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if got.LogLevel != "debug" || len(got.Arrs) != 1 || got.Arrs[0].Name != "sonarr" ||
		got.APIToken != "token" || got.AuthUsername != "admin" || !got.AuthTokenOnly {
		t.Fatalf("configResponse = %s", body)
	}
	if cfg.Arrs[0].Name != "stale" {
		t.Fatal("configResponse changed the caller's config")
	}
}

func TestMergeConfigUpdateClearsWhatTheSettingsPageEmpties(t *testing.T) {
	t.Parallel()
	inherit := true
	current := config.Config{
		Debrids: []config.Debrid{{Name: "torbox", APIKey: "tb-key", DownloadAPIKeys: []string{"a", "b"}}},
		Arrs:    []config.Arr{{Name: "sonarr", Host: "http://sonarr", DownloadUncached: &inherit}},
	}
	body := `{"debrids":[{"name":"torbox","download_api_keys":[]}],` +
		`"arrs":[{"name":"sonarr","host":"http://sonarr","download_uncached":null}]}`
	merged, err := mergeConfigUpdate(&current, strings.NewReader(body))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}
	if len(merged.Debrids[0].DownloadAPIKeys) != 0 || merged.Arrs[0].DownloadUncached != nil {
		t.Fatalf("download_api_keys = %v, download_uncached = %v; want both cleared",
			merged.Debrids[0].DownloadAPIKeys, merged.Arrs[0].DownloadUncached)
	}
}

// A new STRM secret sent through the settings save lands in secrets.json, never config.json.
func TestUpdateConfigRotatesStrmSecretIntoSecretsFile(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
	response := httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"strm":{"secret":"rotated-strm-key"}}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	if got := store.Get().Strm.Secret; got != "rotated-strm-key" {
		t.Fatalf("Strm.Secret = %q, want the rotated key", got)
	}
	data, err := os.ReadFile(store.Get().JSONFile())
	if err != nil || strings.Contains(string(data), "rotated-strm-key") {
		t.Fatalf("config.json holds the STRM secret: %s, %v", data, err)
	}
	secrets, err := os.ReadFile(store.Get().SecretsFile())
	if err != nil || !strings.Contains(string(secrets), "rotated-strm-key") {
		t.Fatalf("secrets.json = %q, %v", secrets, err)
	}
	reloaded, err := config.Load(store.Get().Dir(), config.MapEnv(nil))
	if err != nil || reloaded.Strm.Secret != "rotated-strm-key" {
		t.Fatalf("reloaded Strm.Secret = %v, %v", reloaded, err)
	}
}
