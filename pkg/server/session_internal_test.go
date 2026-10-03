package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/sessions"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func TestCredentialChangesInvalidateBrowserSessions(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"password", "token", "mode"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			store := managertest.Store(t, func(cfg *config.Config) { cfg.UseAuth = true })
			cfg := store.Get()
			body := storeInitialCredentials(t, cfg, change)
			s := &Server{config: store, cookie: sessions.NewCookieStore([]byte(cfg.SecretKey()))}
			request := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			request.AddCookie(loginCookie(t, s, body))
			handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			before := httptest.NewRecorder()
			handler.ServeHTTP(before, request)
			if before.Code != http.StatusNoContent {
				t.Fatalf("fresh session status = %d", before.Code)
			}
			changeCredentials(t, s, cfg, change)
			after := httptest.NewRecorder()
			handler.ServeHTTP(after, request)
			if after.Code != http.StatusUnauthorized {
				t.Fatalf("old session status = %d, want 401", after.Code)
			}
		})
	}
}

// storeInitialCredentials saves the pre-change credential and returns the
// login body that matches it.
func storeInitialCredentials(t *testing.T, cfg *config.Config, change string) string {
	t.Helper()
	if change == "token" {
		if err := cfg.SaveAuth(&config.Auth{TokenOnly: true, APIToken: "old-token"}); err != nil {
			t.Fatal(err)
		}
		return `{"password":"old-token"}`
	}
	if err := cfg.SetCredentials("admin", "old-password"); err != nil {
		t.Fatal(err)
	}
	return `{"username":"admin","password":"old-password"}`
}

func loginCookie(t *testing.T, s *Server, body string) *http.Cookie {
	t.Helper()
	login := httptest.NewRecorder()
	s.LoginHandler(login, httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body)))
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d: %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies, want 1", len(cookies))
	}
	return cookies[0]
}

func changeCredentials(t *testing.T, s *Server, cfg *config.Config, change string) {
	t.Helper()
	var err error
	switch change {
	case "password":
		err = cfg.SetCredentials("admin", "new-password")
	case "token":
		_, err = s.refreshAPIToken()
	case "mode":
		err = cfg.SaveAuth(&config.Auth{TokenOnly: true, APIToken: "new-token"})
	}
	if err != nil {
		t.Fatal(err)
	}
}
