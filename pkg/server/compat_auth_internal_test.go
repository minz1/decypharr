package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/server/qbit"
	"github.com/sirrobot01/decypharr/pkg/server/sabnzbd"
)

// compatAuthRequest carries the credentials the way each compat API reads
// them: Basic auth for qBittorrent, ma_username/ma_password for SABnzbd.
func compatAuthRequest(protocol, category, username, password string) *http.Request {
	query := url.Values{"category": {category}}
	path := "/torrents/categories"
	if protocol == "sabnzbd" {
		path = "/api/"
		query.Set("mode", "version")
		query.Set("ma_username", username)
		query.Set("ma_password", password)
	}
	req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	req.SetBasicAuth(username, password)
	return req
}

func TestCompatibilityAPIsAuthenticateBeforeProbing(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.UseAuth = true
		cfg.Auth = &config.Auth{APIToken: "server-token", TokenOnly: true}
	})

	var probes atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		_, _ = w.Write([]byte(`{"appName":"Sonarr"}`))
	}))
	t.Cleanup(endpoint.Close)
	mgr.Arr().AddOrUpdate(arr.Arr{Name: "manual", Host: endpoint.URL, Token: "arr-token", Source: arr.SourceManual})
	mgr.Arr().AddOrUpdate(arr.Arr{Name: "discovered", Host: endpoint.URL, Token: "arr-token", Source: arr.SourceAuto})

	for _, protocol := range []struct {
		name    string
		handler http.Handler
	}{
		{name: "qbit", handler: qbit.New(mgr, store, zerolog.Nop()).Routes()},
		{name: "sabnzbd", handler: sabnzbd.New(mgr, store, zerolog.Nop()).Routes()},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name, category, username, password string
				wantStatus                         int
			}{
				{"untrusted endpoint", "unknown", endpoint.URL, "arr-token", http.StatusUnauthorized},
				{"wrong configured token", "manual", endpoint.URL, "wrong", http.StatusUnauthorized},
				{"wrong configured host", "manual", "http://untrusted.invalid", "arr-token", http.StatusUnauthorized},
				{"discovered credentials", "discovered", endpoint.URL, "arr-token", http.StatusUnauthorized},
				{"configured credentials", "manual", endpoint.URL, "arr-token", http.StatusOK},
				{"local token", "manual", "", "server-token", http.StatusOK},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					response := httptest.NewRecorder()
					protocol.handler.ServeHTTP(
						response,
						compatAuthRequest(protocol.name, tc.category, tc.username, tc.password),
					)
					if response.Code != tc.wantStatus {
						t.Fatalf("status = %d, want %d: %s", response.Code, tc.wantStatus, response.Body.String())
					}
					if got := probes.Load(); got != 0 {
						t.Fatalf("authentication sent %d probe requests", got)
					}
				})
			}
		})
	}
	response := httptest.NewRecorder()
	(&Server{manager: mgr, config: store}).handleGetConfig(
		response,
		httptest.NewRequest(http.MethodGet, "/api/config", nil),
	)
	var publicConfig map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &publicConfig); err != nil {
		t.Fatal(err)
	}
	if _, exposed := publicConfig["session_secret"]; exposed {
		t.Fatal("the config API exposed the session signing key")
	}
}
