package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// The arr's import webhook names the download it just imported; that download is the decypharr entry to index.
func TestArrImportEntry(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]string{
		`{"eventType":"Download","downloadId":"ABCDEF0123"}`: "abcdef0123",
		`{"eventType":"Download","downloadId":"nzb-uuid-1"}`: "nzb-uuid-1",
		`{"eventType":"Test"}`:                               "",
		`{"eventType":"Grab","downloadId":"ABCDEF0123"}`:     "",
		`{"eventType":"Download"}`:                           "",
	} {
		got, err := arrImportEntry(strings.NewReader(body))
		if err != nil || got != want {
			t.Errorf("arrImportEntry(%s) = %q, %v; want %q", body, got, err, want)
		}
	}
	if _, err := arrImportEntry(strings.NewReader("{")); err == nil {
		t.Error("arrImportEntry(malformed) = nil error")
	}
}

func TestArrWebhookAnswers(t *testing.T) {
	t.Parallel()
	mgr, _ := managertest.New(t, func(cfg *config.Config) {
		cfg.Arrs = []config.Arr{
			{Name: "sonarr", Host: "http://sonarr.invalid", Token: "t", ImportWebhook: true},
			{Name: "radarr", Host: "http://radarr.invalid", Token: "t"},
		}
	})
	if err := mgr.Storage().AddOrUpdate(&storage.Entry{
		Protocol: config.ProtocolTorrent, InfoHash: "abc", Name: "Show.S01E01", AddedOn: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{manager: mgr, logger: zerolog.Nop()}
	router := chi.NewRouter()
	router.Post("/webhooks/arr/{name}", s.handleArrWebhook)
	for _, tc := range []struct {
		arr, body string
		want      int
	}{
		{"sonarr", `{"eventType":"Test"}`, http.StatusOK},
		{"sonarr", `{"eventType":"Download","downloadId":"ABC"}`, http.StatusAccepted},
		{"sonarr", `{"eventType":"Download","downloadId":"foreign"}`, http.StatusOK},
		{"sonarr", `not json`, http.StatusBadRequest},
		{"radarr", `{"eventType":"Test"}`, http.StatusNotFound},
		{"lidarr", `{"eventType":"Test"}`, http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhooks/arr/"+tc.arr, strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Errorf("POST %s %s = %d, want %d", tc.arr, tc.body, w.Code, tc.want)
		}
	}
}

// Arr webhooks configured from tofu can only send basic auth, so the API token is also accepted as its password.
func TestAPITokenAcceptsBasicAuthPassword(t *testing.T) {
	t.Parallel()
	_, store := managertest.New(t, func(cfg *config.Config) {
		cfg.UseAuth = true
		cfg.Auth = &config.Auth{APIToken: "server-token", TokenOnly: true}
	})
	s := &Server{config: store}
	for name, tc := range map[string]struct {
		set  func(*http.Request)
		want bool
	}{
		"bearer":               {func(r *http.Request) { r.Header.Set("Authorization", "Bearer server-token") }, true},
		"basic with token":     {func(r *http.Request) { r.SetBasicAuth("arr", "server-token") }, true},
		"basic wrong password": {func(r *http.Request) { r.SetBasicAuth("arr", "nope") }, false},
		"none":                 {func(*http.Request) {}, false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/arr/sonarr", nil)
		tc.set(r)
		if got := s.isValidAPIToken(r); got != tc.want {
			t.Errorf("%s: isValidAPIToken = %v, want %v", name, got, tc.want)
		}
	}
}

func TestArrWebhookRequiresAuth(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.UseAuth = true
		cfg.Auth = &config.Auth{APIToken: "server-token", TokenOnly: true}
		cfg.Arrs = []config.Arr{{Name: "sonarr", Host: "http://sonarr.invalid", Token: "t", ImportWebhook: true}}
	})
	srv := New(mgr, store, logger.Discard())
	for name, tc := range map[string]struct {
		set  func(*http.Request)
		want int
	}{
		"no credentials":   {func(*http.Request) {}, http.StatusUnauthorized},
		"basic with token": {func(r *http.Request) { r.SetBasicAuth("sonarr", "server-token") }, http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/arr/sonarr", strings.NewReader(`{"eventType":"Test"}`))
		tc.set(r)
		w := httptest.NewRecorder()
		srv.router.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: POST = %d, want %d", name, w.Code, tc.want)
		}
	}
}
