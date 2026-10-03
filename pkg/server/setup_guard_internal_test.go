package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/sessions"

	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// A stored credential must survive setup becoming "incomplete" again (any
// config edit that fails Validate, e.g. removing the last provider). Before
// the guard, anyone could then POST /skip-auth to turn auth off, or
// /api/setup/complete to overwrite the credentials.
func TestSetupEndpointsProtectStoredCredentials(t *testing.T) {
	t.Parallel()
	store := managertest.Store(t, nil)
	cfg := store.Get()
	if err := cfg.SetCredentials("admin", "secret"); err != nil {
		t.Fatal(err)
	}
	if cfg.SetupComplete() == nil {
		t.Fatal("setup should be incomplete: no provider configured")
	}
	s := newTestServer(t, store)
	s.cookie = sessions.NewCookieStore([]byte("test-secret"))

	w := httptest.NewRecorder()
	s.skipAuthHandler(w, httptest.NewRequest(http.MethodPost, "/skip-auth", nil))
	if w.Code != http.StatusUnauthorized || !store.Get().UseAuth {
		t.Fatalf(
			"unauthenticated skip-auth = %d, UseAuth = %t; want 401 and auth still on",
			w.Code,
			store.Get().UseAuth,
		)
	}

	w = httptest.NewRecorder()
	body := `{"auth":{"username":"evil","password":"evil"},"debrid":{"provider":"realdebrid","api_key":"k"},` +
		`"download":{"download_folder":"` + t.TempDir() + `"}}`
	s.setupCompleteHandler(w, httptest.NewRequest(http.MethodPost, "/api/setup/complete", strings.NewReader(body)))
	if w.Code != http.StatusUnauthorized || !store.Get().VerifyAuth("admin", "secret") {
		t.Fatalf("unauthenticated setup/complete = %d; want 401 and the old credentials intact", w.Code)
	}

	login := httptest.NewRecorder()
	s.LoginHandler(login, httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"username":"admin","password":"secret"}`)))
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login status %d set %d cookies", login.Code, len(cookies))
	}
	req := httptest.NewRequest(http.MethodPost, "/skip-auth", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	s.skipAuthHandler(w, req)
	if w.Code != http.StatusSeeOther || store.Get().UseAuth {
		t.Fatalf("authenticated skip-auth = %d, UseAuth = %t; want 303 and auth off", w.Code, store.Get().UseAuth)
	}
}

// /register (once closed) and /setup (once complete) redirected to "/",
// escaping a reverse-proxy URL base.
func TestPageRedirectsKeepURLBase(t *testing.T) {
	t.Parallel()
	store := managertest.Store(t, nil)
	if err := store.Get().SetCredentials("admin", "secret"); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, store)
	s.urlBase = "/decypharr/"
	w := httptest.NewRecorder()
	s.RegisterHandler(w, httptest.NewRequest(http.MethodGet, "/decypharr/register", nil))
	if got := w.Header().Get("Location"); got != "/decypharr/" {
		t.Fatalf("closed /register redirected to %q, want /decypharr/", got)
	}
}

// chi never rewrites r.URL.Path, so under a URL base the skip list never
// matched and /base/setup redirected to itself forever.
func TestSetupRedirectHonorsURLBase(t *testing.T) {
	t.Parallel()
	s := &Server{urlBase: "/decypharr/", config: managertest.Store(t, nil)}
	h := s.setupRedirectMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for path, want := range map[string]int{
		"/decypharr/setup":            http.StatusNoContent,
		"/decypharr/login":            http.StatusNoContent,
		"/decypharr/assets/app.js":    http.StatusNoContent,
		"/decypharr/api/setup/finish": http.StatusNoContent,
		"/decypharr/":                 http.StatusSeeOther,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Errorf("GET %s = %d %q, want %d", path, w.Code, w.Header().Get("Location"), want)
		}
	}
}
