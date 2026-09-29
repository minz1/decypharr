package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
)

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if setup is needed
		cfg := config.Get()
		if !cfg.UseAuth {
			next.ServeHTTP(w, r)
			return
		}

		isAPI := s.isAPIRequest(r)

		if cfg.NeedsAuth() {
			if isAPI {
				s.sendJSONError(w, "Authentication setup required", http.StatusUnauthorized)
			} else {
				s.redirectTo(w, r, "/register")
			}
			return
		}

		if !s.isAuthenticated(r) {
			if isAPI {
				s.sendJSONError(
					w,
					"Authentication required. Please provide a valid API token in the Authorization header (Bearer <token>) or authenticate via session cookies.",
					http.StatusUnauthorized,
				)
			} else {
				s.redirectTo(w, r, "/login")
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isAuthenticated reports whether r carries a valid API token or a login
// session minted for the current credentials.
func (s *Server) isAuthenticated(r *http.Request) bool {
	if s.isValidAPIToken(r) {
		return true
	}
	session, err := s.cookie.Get(r, "auth-session")
	if err != nil {
		return false
	}
	auth, _ := session.Values["authenticated"].(bool)
	version, hasVersion := session.Values["auth_version"].(string)
	currentAuth := config.Get().GetAuth()
	return auth && hasVersion && currentAuth != nil && version == currentAuth.SessionVersion
}

// mayConfigureAuth guards the unauthenticated setup endpoints. They exist to
// set the first credential; once one is stored, reaching them unauthenticated
// (e.g. because a later config edit made setup "incomplete" again) must not
// let a caller replace or disable it.
func (s *Server) mayConfigureAuth(r *http.Request, cfg *config.Config) bool {
	return !cfg.UseAuth || cfg.NeedsAuth() || s.isAuthenticated(r)
}

// relPath returns the request path with the URL base stripped. chi routes on
// its own context, so r.URL.Path still carries the base.
func (s *Server) relPath(r *http.Request) string {
	path := r.URL.Path
	if urlBase := strings.TrimSuffix(s.urlBase, "/"); urlBase != "" {
		path = strings.TrimPrefix(path, urlBase)
	}
	return path
}

// isAPIRequest reports whether an authentication failure should be returned as
// JSON instead of redirecting the caller to the login page.
func (s *Server) isAPIRequest(r *http.Request) bool {
	path := s.relPath(r)
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/webhooks/")
}

// sendJSONError sends a JSON error response.
func (s *Server) sendJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	err := json.NewEncoder(w).Encode(map[string]any{
		"error":  message,
		"status": statusCode,
	})
	if err != nil {
		return
	}
}

// setupRedirectMiddleware redirects to /setup if setup is not completed.
func (s *Server) setupRedirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := config.Get()

		// Skip setup check for setup-related routes. /login stays reachable so
		// an instance with stored credentials can authenticate to rerun setup.
		path := s.relPath(r)
		if strings.HasPrefix(path, "/setup") ||
			strings.HasPrefix(path, "/api/setup") ||
			strings.HasPrefix(path, "/login") ||
			strings.HasPrefix(path, "/api/config") ||
			strings.HasPrefix(path, "/assets") ||
			strings.HasPrefix(path, "/images") ||
			path == "/version" {
			next.ServeHTTP(w, r)
			return
		}

		// Check if setup is completed
		if err := cfg.SetupComplete(); err != nil {
			isAPI := s.isAPIRequest(r)
			if isAPI {
				s.sendJSONError(
					w,
					fmt.Sprintf("[error] %s Setup wizard must be completed first. Please visit /setup", err),
					http.StatusServiceUnavailable,
				)
			} else {
				s.redirectTo(w, r, "/setup")
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

// redirectTo redirects to path with the URLBase prefix so reverse-proxy
// deployments at a subpath get correct redirect targets.
func (s *Server) redirectTo(w http.ResponseWriter, r *http.Request, path string) {
	target := strings.TrimSuffix(s.urlBase, "/") + path
	http.Redirect(w, r, target, http.StatusSeeOther)
}
