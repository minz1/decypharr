package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sirrobot01/decypharr/internal/config"
)

// maySkipAuth reports whether auth may be turned off without credentials:
// while the register page is open (auth on, no credential yet), which is
// where the Skip button lives, or before setup is complete.
func maySkipAuth(cfg *config.Config) bool {
	return cfg.NeedsAuth() || cfg.SetupComplete() != nil
}

func (s *Server) skipAuthHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Get()
	if !maySkipAuth(cfg) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !s.mayConfigureAuth(r, cfg) {
		http.Error(w, "authentication required: log in first", http.StatusUnauthorized)
		return
	}
	_, err := s.config.Update(func(next *config.Config) error {
		if !maySkipAuth(next) {
			return fmt.Errorf("setup is already complete")
		}
		next.UseAuth = false
		return nil
	})
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to save config")
		http.Error(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	s.redirectTo(w, r, "/")
}

// isValidAPIToken checks if the request contains a valid API token.
func (s *Server) isValidAPIToken(r *http.Request) bool {
	// Check Authorization header for Bearer token
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	// Support both "Bearer <token>" and "Token <token>" formats
	token, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok {
		token, ok = strings.CutPrefix(authHeader, "Token ")
	}
	if !ok {
		return false
	}

	if token == "" {
		return false
	}

	return s.config.Get().VerifyToken(token)
}

// refreshAPIToken generates a new API token and saves it.
func (s *Server) refreshAPIToken() (string, error) {
	token, err := config.GenerateAPIToken()
	if err != nil {
		return "", err
	}
	_, err = s.config.Update(func(next *config.Config) error {
		auth := next.GetAuth()
		if auth == nil {
			return fmt.Errorf("authentication not configured")
		}
		auth.APIToken = token
		return next.SaveAuth(auth)
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
