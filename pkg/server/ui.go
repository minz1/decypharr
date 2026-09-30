package server

import (
	"encoding/json"
	"fmt"
	"maps"
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
		s.renderPage(w, "layout", "login", "Login", map[string]any{"TokenOnly": tokenOnly})
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
	s.redirectTo(w, r, "/")
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
		s.redirectTo(w, r, "/")
		return
	}

	if r.Method == http.MethodGet {
		s.renderPage(w, "layout", "register", "registerVolume", nil)
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

// renderPage executes templateName with the fields every page reads plus
// extra. Templates that do not show the setup banner ignore SetupError.
func (s *Server) renderPage(w http.ResponseWriter, templateName, page, title string, extra map[string]any) {
	cfg := config.Get()
	data := map[string]any{
		"URLBase":    cfg.URLBase,
		"Page":       page,
		"Title":      title,
		"SetupError": cfg.SetupError(),
	}
	maps.Copy(data, extra)
	if err := s.templates.ExecuteTemplate(w, templateName, data); err != nil {
		s.logger.Warn().Err(err).Str("page", page).Msg("error rendering template")
	}
}

// IndexHandler renders the queue dashboard.
func (s *Server) IndexHandler(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "layout", "index", "Queues", nil)
}

// DownloadHandler renders the add-content page.
func (s *Server) DownloadHandler(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	debrids := make([]string, 0, len(cfg.Debrids))
	for _, d := range cfg.Debrids {
		debrids = append(debrids, d.Name)
	}
	s.renderPage(w, "layout", "download", "Download", map[string]any{
		"Debrids":                 debrids,
		"HasMultiDebrid":          len(debrids) > 1,
		"downloadFolder":          cfg.DownloadFolder,
		"alwaysRemoveTrackerURLS": cfg.AlwaysRmTrackerUrls,
	})
}

// RepairHandler renders the repair page.
func (s *Server) RepairHandler(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "layout", "repair", "Repair", nil)
}

// ReacquireHandler renders the Arr reacquisition page.
func (s *Server) ReacquireHandler(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "layout", "reacquire", "Reacquire", nil)
}

// ConfigHandler renders the settings page.
func (s *Server) ConfigHandler(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "layout", "config", "Config", nil)
}

// StatsHandler renders the statistics page.
func (s *Server) StatsHandler(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "layout", "stats", "Statistics", nil)
}

// BrowseHandler renders the file browser.
func (s *Server) BrowseHandler(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "layout", "browse", "Browse Torrents", nil)
}
