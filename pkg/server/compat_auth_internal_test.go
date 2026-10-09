package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
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
		query.Set("apikey", "legacy-placeholder")
	}
	req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	req.SetBasicAuth(username, password)
	return req
}

type compatAuthFixture struct {
	mgr      *manager.Manager
	store    *config.Store
	endpoint *httptest.Server
	probes   *atomic.Int64
}

// newCompatAuthFixture enables auth, saves one auto-sourced arr, and registers
// a manual and a discovered arr pointing at a counting endpoint.
func newCompatAuthFixture(t *testing.T) compatAuthFixture {
	t.Helper()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.UseAuth = true
		cfg.Auth = &config.Auth{APIToken: "server-token", TokenOnly: true}
		cfg.Arrs = []config.Arr{{Name: "saved", Host: "http://saved.invalid", Token: "saved-token", Source: "auto"}}
	})

	probes := &atomic.Int64{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		_, _ = w.Write([]byte(`{"appName":"Sonarr"}`))
	}))
	t.Cleanup(endpoint.Close)
	mgr.Arr().AddOrUpdate(arr.Arr{Name: "manual", Host: endpoint.URL, Token: "arr-token", Source: arr.SourceManual})
	mgr.Arr().AddOrUpdate(arr.Arr{
		Name: "discovered", Host: endpoint.URL + "/discovered", Token: "arr-token", Source: arr.SourceAuto,
	})
	return compatAuthFixture{mgr: mgr, store: store, endpoint: endpoint, probes: probes}
}

func TestCompatibilityAPIsAuthenticateBeforeProbing(t *testing.T) {
	t.Parallel()
	f := newCompatAuthFixture(t)
	mgr, store, endpoint := f.mgr, f.store, f.endpoint

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
				{"discovered credentials", "discovered", endpoint.URL + "/discovered", "arr-token", http.StatusUnauthorized},
				{"discovered credentials without category", "", endpoint.URL + "/discovered", "arr-token", http.StatusUnauthorized},
				{"configured credentials", "manual", endpoint.URL, "arr-token", http.StatusOK},
				{"configured credentials without category", "", endpoint.URL, "arr-token", http.StatusOK},
				{"saved auto credentials", "saved", "http://saved.invalid", "saved-token", http.StatusOK},
				{"saved auto credentials without category", "", "http://saved.invalid", "saved-token", http.StatusOK},
				{"wrong saved token", "saved", "http://saved.invalid", "wrong", http.StatusUnauthorized},
				{"wrong saved category", "manual", "http://saved.invalid", "saved-token", http.StatusUnauthorized},
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
					if got := f.probes.Load(); got != 0 {
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

func TestQBitLoginAndCookieWithoutCategory(t *testing.T) {
	t.Parallel()
	f := newCompatAuthFixture(t)
	routes := qbit.New(f.mgr, f.store, zerolog.Nop()).Routes()
	for _, credentials := range []struct{ username, password string }{
		{f.endpoint.URL, "arr-token"},
		{"http://saved.invalid", "saved-token"},
		{"client", "server-token"},
	} {
		form := url.Values{"username": {credentials.username}, "password": {credentials.password}}
		loginReq := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(form.Encode()))
		loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		login := httptest.NewRecorder()
		routes.ServeHTTP(login, loginReq)
		if login.Code != http.StatusOK || login.Body.String() != "Ok." {
			t.Fatalf("login status = %d: %s", login.Code, login.Body.String())
		}
		cookies := login.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != "SID" {
			t.Fatalf("login cookies = %v, want SID", cookies)
		}
		for _, path := range []string{"/app/preferences", "/torrents/categories"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(cookies[0])
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
			}
		}
	}
	if got := f.probes.Load(); got != 0 {
		t.Fatalf("authentication sent %d probe requests", got)
	}
}

func TestQBitBearerToken(t *testing.T) {
	t.Parallel()
	f := newCompatAuthFixture(t)
	routes := qbit.New(f.mgr, f.store, zerolog.Nop()).Routes()
	for _, token := range []string{"server-token", "wrong", ""} {
		req := httptest.NewRequest(http.MethodGet, "/app/preferences", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		want := http.StatusUnauthorized
		if token == "server-token" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("token %q: status = %d, want %d", token, response.Code, want)
		}
	}
}

func TestSABnzbdAPIKey(t *testing.T) {
	t.Parallel()
	f := newCompatAuthFixture(t)
	routes := sabnzbd.New(f.mgr, f.store, zerolog.Nop()).Routes()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, token := range []string{"server-token", "wrong", ""} {
			values := url.Values{"mode": {"version"}, "apikey": {token}}
			req := httptest.NewRequest(method, "/api/?"+values.Encode(), nil)
			if method == http.MethodPost {
				req = httptest.NewRequest(method, "/api/", strings.NewReader(values.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, req)
			want := http.StatusUnauthorized
			if token == "server-token" {
				want = http.StatusOK
			}
			if response.Code != want {
				t.Fatalf("%s token %q: status = %d, want %d", method, token, response.Code, want)
			}
		}
	}
}

// SABnzbd always sends the Arr's URL as ma_username, so an arr key without a
// host is not a credential there, unlike a qBittorrent Bearer key.
func TestSABnzbdRejectsArrKeyWithoutHost(t *testing.T) {
	t.Parallel()
	f := newCompatAuthFixture(t)
	routes := sabnzbd.New(f.mgr, f.store, zerolog.Nop()).Routes()
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, compatAuthRequest("sabnzbd", "manual", "", "arr-token"))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}
