package qbit

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func newAuthenticationTestQBit(t *testing.T) *QBit {
	t.Helper()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	return &QBit{manager: mgr, config: store}
}

func TestAuthenticateDoesNotOverwriteArrWithClientCredentials(t *testing.T) {
	t.Parallel()
	q := newAuthenticationTestQBit(t)
	existing := arr.Arr{Name: "whisparr", Host: "http://whisparr:6969", Token: "arr-api-key"}
	q.manager.Arr().AddOrUpdate(existing)

	got, err := q.authenticate(t.Context(), "whisparr", "homarr-user", "homarr-password")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.Host != existing.Host || got.Token != existing.Token {
		t.Fatalf(
			"authenticated Arr = host %q token %q, want host %q token %q",
			got.Host,
			got.Token,
			existing.Host,
			existing.Token,
		)
	}
	stored, _ := q.manager.Arr().Get("whisparr")
	if stored.Host != existing.Host || stored.Token != existing.Token {
		t.Fatalf(
			"stored Arr = host %q token %q, want host %q token %q",
			stored.Host,
			stored.Token,
			existing.Host,
			existing.Token,
		)
	}
}

func TestAuthenticateDiscoversValidatedArrCredentials(t *testing.T) {
	t.Parallel()
	q := newAuthenticationTestQBit(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/system/status" || r.Header.Get("X-Api-Key") != "arr-api-key" {
			http.Error(w, "unexpected Arr validation request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"appName":"Sonarr"}`))
	}))
	t.Cleanup(server.Close)

	got, err := q.authenticate(t.Context(), "whisparr", server.URL, "arr-api-key")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.Host != server.URL || got.Token != "arr-api-key" || got.Source != arr.SourceAuto {
		t.Fatalf("authenticated Arr = %#v", got)
	}
	if got.Type != arr.Sonarr {
		t.Fatalf("Arr type = %q, want the type the instance reported", got.Type)
	}
	stored, ok := q.manager.Arr().Get("whisparr")
	if !ok || stored.Host != server.URL || stored.Token != "arr-api-key" {
		t.Fatalf("stored Arr = %#v", stored)
	}
}

func TestPreferencesRequireAuthentication(t *testing.T) {
	t.Parallel()
	q := newAuthenticationTestQBit(t)
	cfg := q.config.Get()
	cfg.UseAuth = true
	cfg.Auth = &config.Auth{APIToken: "api-token", TokenOnly: true}
	routes := q.Routes()
	for _, token := range []string{"", "wrong", "api-token"} {
		req := httptest.NewRequest(http.MethodGet, "/app/preferences", nil)
		if token != "" {
			req.SetBasicAuth("client", token)
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		want := http.StatusUnauthorized
		if token == "api-token" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("token %q: status = %d, want %d", token, response.Code, want)
		}
	}
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app/version", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("public version status = %d, want 200", response.Code)
	}
}

// TestDecodeAuthHeader covers the fix for the slice-bounds-out-of-range panic
// at pkg/server/qbit/context.go:60-62. When the base64-decoded payload contains
// no colon, [strings.LastIndex] returns -1 and the subsequent slice expression
// `bearer[:colonIndex]` panics with "slice bounds out of range [:-1]".
//
// Pre-fix the "no colon" cases panic; post-fix they return a clean error and
// let the caller respond with 401 instead of crashing the request handler
// (chi's Recoverer middleware catches the panic, but the goroutine traceback
// is logged on every occurrence).
func TestDecodeAuthHeader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		header       string
		wantErr      bool
		wantUser     string
		wantPass     string
		mustNotPanic bool // documents the regression we're guarding against
	}{
		{
			name:     "well-formed Basic auth",
			header:   "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:hunter2")),
			wantErr:  false,
			wantUser: "alice",
			wantPass: "hunter2",
		},
		{
			name:     "well-formed with colon in password",
			header:   "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:hunt:er2")),
			wantErr:  false,
			wantUser: "alice:hunt", // strings.LastIndex => split on the last colon
			wantPass: "er2",
		},
		{
			// Empty payload — base64 of "" is "", decoded back is "". No colon.
			// PRE-FIX: panic.
			name:         "empty payload (the panic case)",
			header:       "Basic ",
			wantErr:      true,
			mustNotPanic: true,
		},
		{
			// Garbage that decodes successfully but has no colon.
			// PRE-FIX: panic.
			name:         "no-colon decoded bytes",
			header:       "Basic " + base64.StdEncoding.EncodeToString([]byte("just-a-token-no-colon")),
			wantErr:      true,
			mustNotPanic: true,
		},
		{
			// qBittorrent API keys, as Sonarr and Radarr send them.
			name:     "Bearer token",
			header:   "Bearer sonarr-key",
			wantPass: "sonarr-key",
		},
		{
			name:    "unsupported scheme",
			header:  "Token " + base64.StdEncoding.EncodeToString([]byte("alice:hunter2")),
			wantErr: true,
		},
		{
			name:    "non-base64 payload",
			header:  "Basic !!!not-base64!!!",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A panic fails the test on its own; mustNotPanic documents which
			// cases used to panic.
			user, pass, err := decodeAuthHeader(tt.header)
			if (err != nil) != tt.wantErr {
				t.Errorf("header=%q: err = %v, wantErr %t (user=%q, pass=%q)", tt.header, err, tt.wantErr, user, pass)
			}
			if tt.wantUser != "" && user != tt.wantUser {
				t.Errorf("user mismatch: got %q want %q", user, tt.wantUser)
			}
			if tt.wantPass != "" && pass != tt.wantPass {
				t.Errorf("pass mismatch: got %q want %q", pass, tt.wantPass)
			}
		})
	}
}

// newUseAuthTestQBit returns a QBit with authentication on, decypharr API
// token "api-token", a configured sonarr and radarr, and an arr registered
// automatically from client credentials.
func newUseAuthTestQBit(t *testing.T) *QBit {
	t.Helper()
	q := newAuthenticationTestQBit(t)
	cfg := q.config.Get()
	cfg.UseAuth = true
	cfg.Auth = &config.Auth{APIToken: "api-token", TokenOnly: true}
	q.manager.Arr().
		AddOrUpdate(arr.Arr{Name: "sonarr", Host: "http://sonarr:8989", Token: "sonarr-key", Source: arr.SourceManual})
	q.manager.Arr().
		AddOrUpdate(arr.Arr{Name: "radarr", Host: "http://radarr:7878", Token: "radarr-key", Source: arr.SourceManual})
	q.manager.Arr().
		AddOrUpdate(arr.Arr{Name: "lidarr", Host: "http://lidarr:8686", Token: "lidarr-key", Source: arr.SourceAuto})
	return q
}

// With auth on, an arr authenticates with its own host and API key (Basic or
// SID) or with its API key alone (a Bearer API key). Requests that name a
// category must carry that category's arr credentials; requests without one,
// such as login and app/preferences, accept any configured arr's. Arrs registered from client credentials are never trusted.
func TestAuthenticateArrCredentialsWithAuthOn(t *testing.T) {
	t.Parallel()
	q := newUseAuthTestQBit(t)
	tests := []struct {
		name, category, username, password string
		ok                                 bool
	}{
		{"bearer key for its category", "sonarr", "", "sonarr-key", true},
		{"bearer key without a category", "", "", "sonarr-key", true},
		{"bearer key for an unknown category", "tv", "", "sonarr-key", false},
		{"bearer key for another arr's category", "radarr", "", "sonarr-key", false},
		{"bearer decypharr API token", "radarr", "", "api-token", true},
		{"bearer unknown key", "", "", "nope", false},
		{"bearer key of an auto-registered arr", "", "", "lidarr-key", false},
		{"host and key for its category", "sonarr", "http://sonarr:8989", "sonarr-key", true},
		{"host and key without a category", "", "http://sonarr:8989", "sonarr-key", true},
		{"host with another arr's key", "", "http://sonarr:8989", "radarr-key", false},
		{"empty key", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := q.authenticate(t.Context(), tt.category, tt.username, tt.password)
			if (err == nil) != tt.ok {
				t.Fatalf(
					"authenticate(%q, %q, %q) err = %v, want ok %t",
					tt.category,
					tt.username,
					tt.password,
					err,
					tt.ok,
				)
			}
		})
	}
}

// Sonarr and Radarr with a qBittorrent API key set send no username or
// password, only "Authorization: Bearer <key>", and call category-less
// endpoints such as app/preferences when testing the client.
func TestBearerAPIKeyReachesAuthenticatedRoutes(t *testing.T) {
	t.Parallel()
	q := newUseAuthTestQBit(t)
	routes := q.Routes()
	for header, want := range map[string]int{
		"Bearer sonarr-key": http.StatusOK,
		"Bearer api-token":  http.StatusOK,
		"Bearer wrong":      http.StatusUnauthorized,
		"":                  http.StatusUnauthorized,
	} {
		req := httptest.NewRequest(http.MethodGet, "/app/preferences", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("Authorization %q: status = %d, want %d", header, response.Code, want)
		}
	}
}

// Sonarr and Radarr log in with their host and API key and no category, then
// send the SID cookie on every request, including category-less ones.
func TestArrLoginWithoutCategoryMintsUsableSID(t *testing.T) {
	t.Parallel()
	routes := newUseAuthTestQBit(t).Routes()
	form := url.Values{"username": {"http://sonarr:8989"}, "password": {"sonarr-key"}}
	login := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(form.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	routes.ServeHTTP(loginResponse, login)
	cookies := loginResponse.Result().Cookies()
	if loginResponse.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login: status = %d, cookies = %d, want 200 and a SID", loginResponse.Code, len(cookies))
	}
	req := httptest.NewRequest(http.MethodGet, "/app/preferences", nil)
	req.AddCookie(cookies[0])
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("app/preferences with the SID: status = %d, want 200", response.Code)
	}
}
