package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/sirrobot01/decypharr/internal/config"
)

// SetupWizardResponse represents the response from setup wizard.
type SetupWizardResponse struct {
	Success    bool   `json:"success"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	RedirectTo string `json:"redirect_to,omitempty"`
	// APIToken is returned once, in token-only mode, so the user can copy it.
	APIToken string `json:"api_token,omitempty"`
}

// SetupHandler renders the setup wizard page.
func (s *Server) SetupHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Get()

	if err := cfg.SetupComplete(); err == nil {
		s.redirectTo(w, r, "/")
		return
	}
	s.renderPage(w, "setup_layout", "setup", "Setup Wizard", nil)
}

// sendSetupError sends an error response.
func (s *Server) sendSetupError(w http.ResponseWriter, message string, err error) {
	response := SetupWizardResponse{
		Success: false,
		Error:   message,
	}
	if err != nil {
		response.Error = fmt.Sprintf("%s: %v", message, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	//nolint:gosec // G117: the API token is shown once, on purpose, after token-only setup
	_ = json.NewEncoder(w).Encode(response)
}

// SetupCompleteRequest represents the complete setup data from frontend.
type SetupCompleteRequest struct {
	Auth struct {
		Username  string `json:"username,omitempty"`
		Password  string `json:"password,omitempty"`
		SkipAuth  bool   `json:"skip_auth,omitempty"`
		TokenOnly bool   `json:"token_only,omitempty"`
	} `json:"auth"`
	Debrid struct {
		Provider string `json:"provider,omitempty"`
		APIKey   string `json:"api_key,omitempty"`
		Skip     bool   `json:"skip_debrid,omitempty"`
	} `json:"debrid"`
	Usenet struct {
		Host              string `json:"host,omitempty"`
		Port              int    `json:"port,omitempty"`
		Username          string `json:"username,omitempty"`
		Password          string `json:"password,omitempty"`
		MaxConnections    int    `json:"max_connections,omitempty"`
		ReaderConnections int    `json:"reader_connections,omitempty"`
		SSL               bool   `json:"ssl,omitempty"`
		Skip              bool   `json:"skip_usenet,omitempty"`
	} `json:"usenet"`
	Download struct {
		DownloadFolder string `json:"download_folder"`
	} `json:"download"`
	Mount struct {
		MountType        string `json:"mount_type"`
		MountPath        string `json:"mount_path"`
		CacheDir         string `json:"cache_dir"`
		RcloneBufferSize string `json:"rclone_buffer_size,omitempty"`
	} `json:"mount"`
}

// setupCompleteHandler handles the complete setup in a single request.
func (s *Server) setupCompleteHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Get()
	// Prevent re-running setup once it has already been completed
	if err := cfg.SetupComplete(); err == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !s.mayConfigureAuth(r, cfg) {
		http.Error(w, "authentication required: log in first, then rerun setup", http.StatusUnauthorized)
		return
	}

	var req SetupCompleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req); err != nil {
		s.sendSetupError(w, "Invalid request format", err)
		return
	}

	hasDebrid := !req.Debrid.Skip && req.Debrid.Provider != "" && req.Debrid.APIKey != ""
	hasUsenet := !req.Usenet.Skip && req.Usenet.Host != "" && req.Usenet.Port > 0 && req.Usenet.Username != "" &&
		req.Usenet.Password != ""

	if !hasDebrid && !hasUsenet {
		s.sendSetupError(w, "Please configure at least one Debrid or Usenet provider", nil)
		return
	}

	updated, err := s.config.Update(func(cfg *config.Config) error {
		if err := cfg.SetupComplete(); err == nil {
			return errors.New("setup is already complete")
		}
		return applySetup(cfg, &req, hasDebrid, hasUsenet)
	})
	if err != nil {
		s.sendSetupError(w, "Failed to save configuration", err)
		return
	}
	cfg = updated

	// Trigger manager restart to apply new config
	go s.Restart()

	response := SetupWizardResponse{
		Success:    true,
		Message:    "Setup completed successfully! Restarting services...",
		RedirectTo: "/",
	}
	if auth := cfg.GetAuth(); auth != nil && auth.TokenOnly {
		response.APIToken = auth.APIToken
	}

	w.Header().Set("Content-Type", "application/json")
	//nolint:gosec // G117: the API token is shown once, on purpose, after token-only setup
	_ = json.NewEncoder(w).Encode(response)
}

// Defaults the wizard fills in when the user leaves them unset.
const (
	defaultProviderConnections = 30
	defaultReaderConnections   = 15
	defaultMaxActiveDownloads  = 5
	maxPort                    = 65535
)

// isSetupDebridProvider reports whether the wizard can configure name.
func isSetupDebridProvider(name string) bool {
	switch name {
	case "realdebrid", "alldebrid", "debridlink", "torbox", "premiumize":
		return true
	}
	return false
}

// applySetup writes the wizard's choices into cfg. It runs inside
// config.Update, so a returned error discards every change.
func applySetup(cfg *config.Config, req *SetupCompleteRequest, hasDebrid, hasUsenet bool) error {
	if err := applySetupDebrid(cfg, req, hasDebrid); err != nil {
		return err
	}
	if err := applySetupUsenet(cfg, req, hasUsenet); err != nil {
		return err
	}
	if req.Download.DownloadFolder == "" {
		return errors.New("download folder is required")
	}
	// Shared with the Arr containers that import from it.
	//nolint:gosec // G301: media folder read by other users/containers
	if err := os.MkdirAll(req.Download.DownloadFolder, 0o755); err != nil {
		return fmt.Errorf("failed to create download folder: %w", err)
	}
	cfg.DownloadFolder = req.Download.DownloadFolder

	if len(cfg.Categories) == 0 {
		cfg.Categories = []string{"sonarr", "radarr"}
	}
	if cfg.MaxActiveDownloads == 0 {
		cfg.MaxActiveDownloads = defaultMaxActiveDownloads
	}
	if err := applySetupMount(cfg, req); err != nil {
		return err
	}
	if err := cfg.SetupComplete(); err != nil {
		return err
	}
	return applySetupAuth(cfg, req)
}

func applySetupDebrid(cfg *config.Config, req *SetupCompleteRequest, hasDebrid bool) error {
	if !hasDebrid {
		cfg.Debrids = nil
		return nil
	}
	if !isSetupDebridProvider(req.Debrid.Provider) {
		return errors.New("invalid debrid provider")
	}
	debrid := config.Debrid{
		Provider:         req.Debrid.Provider,
		Name:             req.Debrid.Provider,
		APIKey:           req.Debrid.APIKey,
		DownloadAPIKeys:  []string{req.Debrid.APIKey},
		DownloadUncached: false,
		RateLimit:        config.DefaultRateLimit,
	}
	if len(cfg.Debrids) == 0 {
		cfg.Debrids = []config.Debrid{debrid}
	} else {
		cfg.Debrids[0] = debrid
	}
	return nil
}

func applySetupUsenet(cfg *config.Config, req *SetupCompleteRequest, hasUsenet bool) error {
	if !hasUsenet {
		cfg.Usenet.Providers = nil
		return nil
	}
	if req.Usenet.Port < 1 || req.Usenet.Port > maxPort {
		return errors.New("usenet port must be between 1 and 65535")
	}
	providerMax := req.Usenet.MaxConnections
	if providerMax <= 0 {
		providerMax = defaultProviderConnections
	}
	readerConnections := req.Usenet.ReaderConnections
	if readerConnections <= 0 {
		readerConnections = defaultReaderConnections
	}
	cfg.Usenet.Providers = []config.UsenetProvider{{
		Host:           req.Usenet.Host,
		Port:           req.Usenet.Port,
		Username:       req.Usenet.Username,
		Password:       req.Usenet.Password,
		MaxConnections: providerMax,
		SSL:            req.Usenet.SSL,
		Priority:       1,
	}}
	cfg.Usenet.MaxConnections = readerConnections
	cfg.Usenet.ProcessingMaxConnections = readerConnections
	return nil
}

func applySetupMount(cfg *config.Config, req *SetupCompleteRequest) error {
	cfg.Mount.Type = config.MountType(req.Mount.MountType)
	switch req.Mount.MountType {
	case "dfs":
		cfg.Mount.MountPath = req.Mount.MountPath
		if err := os.MkdirAll(req.Mount.CacheDir, 0o750); err != nil {
			return fmt.Errorf("failed to create cache directory: %w", err)
		}
		cfg.Mount.DFS.CacheDir = req.Mount.CacheDir
		cfg.Mount.DFS.ChunkSize = cmp.Or(cfg.Mount.DFS.ChunkSize, "8MB")
		cfg.Mount.DFS.ReadAheadSize = cmp.Or(cfg.Mount.DFS.ReadAheadSize, "32MB")
		cfg.Mount.DFS.CacheExpiry = cmp.Or(cfg.Mount.DFS.CacheExpiry, "24h")
	case "rclone":
		cfg.Mount.MountPath = req.Mount.MountPath
		cfg.Mount.Rclone.CacheDir = cmp.Or(req.Mount.CacheDir, cfg.Mount.Rclone.CacheDir)
		cfg.Mount.Rclone.VfsCacheMode = cmp.Or(cfg.Mount.Rclone.VfsCacheMode, "full")
		cfg.Mount.Rclone.DirCacheTime = cmp.Or(cfg.Mount.Rclone.DirCacheTime, "5m")
	}
	return nil
}

func applySetupAuth(cfg *config.Config, req *SetupCompleteRequest) error {
	switch {
	case req.Auth.SkipAuth:
		cfg.UseAuth = false
	case req.Auth.TokenOnly:
		// Update creates the API token before it publishes the configuration.
		cfg.UseAuth = true
		if err := cfg.SaveAuth(&config.Auth{TokenOnly: true}); err != nil {
			return fmt.Errorf("failed to save authentication: %w", err)
		}
	case req.Auth.Username != "" && req.Auth.Password != "":
		if err := cfg.SetCredentials(req.Auth.Username, req.Auth.Password); err != nil {
			return fmt.Errorf("failed to save authentication: %w", err)
		}
	}
	return nil
}
