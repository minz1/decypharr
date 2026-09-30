package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sirrobot01/decypharr/internal/config"
)

// maxJSONBody caps the JSON bodies of the unauthenticated login and setup
// endpoints.
const maxJSONBody = 1 << 20

// LoginHandler serves the login page and exchanges credentials (or, in
// token-only mode, the API token) for a session cookie.
func (s *Server) LoginHandler(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	if cfg.NeedsAuth() {
		s.redirectTo(w, r, "/register")
		return
	}
	auth := cfg.GetAuth()
	tokenOnly := auth != nil && auth.TokenOnly

	if r.Method == http.MethodGet {
		data := map[string]any{
			"URLBase":   cfg.URLBase,
			"Page":      "login",
			"Title":     "Login",
			"TokenOnly": tokenOnly,
		}
		err := s.templates.ExecuteTemplate(w, "layout", data)
		if err != nil {
			s.logger.Warn().Err(err).Msg("error rendering /login template")
		}
		return
	}

	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&credentials); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	username := credentials.Username
	sessionVersion := ""
	if auth != nil {
		sessionVersion = auth.SessionVersion
	}
	ok := config.VerifyAuth(credentials.Username, credentials.Password)
	if !ok && tokenOnly {
		// Token-only mode has no password, so the API token takes its place.
		// This is the only way into the UI; without it the mode would lock the
		// user out of their own instance.
		ok = config.VerifyToken(credentials.Password)
		username = "token"
	}
	if !ok {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	session, _ := s.cookie.Get(r, "auth-session")
	session.Values["authenticated"] = true
	session.Values["username"] = username
	session.Values["auth_version"] = sessionVersion
	if err := session.Save(r, w); err != nil {
		http.Error(w, "Error saving session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) RegisterHandler(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()

	// Registration exists only to set the first credential. Once auth is
	// configured — including token-only mode, which never has a password — it
	// must stay closed, or anyone could overwrite the stored credentials.
	if !cfg.NeedsAuth() {
		if r.Method == http.MethodPost {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodGet {
		data := map[string]any{
			"URLBase": cfg.URLBase,
			"Page":    "register",
			"Title":   "registerVolume",
		}
		err := s.templates.ExecuteTemplate(w, "layout", data)
		if err != nil {
			s.logger.Warn().Err(err).Msg("error rendering /register template")
		}
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	confirmPassword := r.FormValue("confirmPassword")

	if password != confirmPassword {
		http.Error(w, "Passwords do not match", http.StatusBadRequest)
		return
	}

	updated, err := config.Update(func(next *config.Config) error {
		if !next.NeedsAuth() {
			return fmt.Errorf("registration is closed")
		}
		return next.SetCredentials(username, password)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create a session
	session, _ := s.cookie.Get(r, "auth-session")
	session.Values["authenticated"] = true
	session.Values["username"] = username
	session.Values["auth_version"] = updated.GetAuth().SessionVersion
	if saveErr := session.Save(r, w); saveErr != nil {
		http.Error(w, "Error saving session", http.StatusInternalServerError)
		return
	}

	s.redirectTo(w, r, "/")
}

func (s *Server) IndexHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase":    cfg.URLBase,
		"Page":       "index",
		"Title":      "Queues",
		"SetupError": cfg.SetupError(),
	}
	err := s.templates.ExecuteTemplate(w, "layout", data)
	if err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /index template")
	}
}

func (s *Server) DownloadHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	debrids := make([]string, 0)
	for _, d := range cfg.Debrids {
		debrids = append(debrids, d.Name)
	}
	data := map[string]any{
		"URLBase":                 cfg.URLBase,
		"Page":                    "download",
		"Title":                   "Download",
		"Debrids":                 debrids,
		"HasMultiDebrid":          len(debrids) > 1,
		"downloadFolder":          cfg.DownloadFolder,
		"alwaysRemoveTrackerURLS": cfg.AlwaysRmTrackerUrls,
		"SetupError":              cfg.SetupError(),
	}
	err := s.templates.ExecuteTemplate(w, "layout", data)
	if err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /download template")
	}
}

func (s *Server) RepairHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase":    cfg.URLBase,
		"Page":       "repair",
		"Title":      "Repair",
		"SetupError": cfg.SetupError(),
	}
	err := s.templates.ExecuteTemplate(w, "layout", data)
	if err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /repair template")
	}
}

func (s *Server) ReacquireHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase":    cfg.URLBase,
		"Page":       "reacquire",
		"Title":      "Reacquire",
		"SetupError": cfg.SetupError(),
	}
	if err := s.templates.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /reacquire template")
	}
}

func (s *Server) ConfigHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase":    cfg.URLBase,
		"Page":       "config",
		"Title":      "Config",
		"SetupError": cfg.SetupError(),
	}
	err := s.templates.ExecuteTemplate(w, "layout", data)
	if err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /config template")
	}
}

func (s *Server) StatsHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase": cfg.URLBase,
		"Page":    "stats",
		"Title":   "Statistics",
	}
	err := s.templates.ExecuteTemplate(w, "layout", data)
	if err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /stats template")
	}
}

func (s *Server) BrowseHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase":    cfg.URLBase,
		"Page":       "browse",
		"Title":      "Browse Torrents",
		"SetupError": cfg.SetupError(),
	}
	err := s.templates.ExecuteTemplate(w, "layout", data)
	if err != nil {
		s.logger.Warn().Err(err).Msg("error rendering /browse template")
	}
}
