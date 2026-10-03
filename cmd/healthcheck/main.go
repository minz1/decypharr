package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// HealthStatus represents the status of various components.
type HealthStatus struct {
	QbitAPI       bool `json:"qbit_api"`
	WebUI         bool `json:"web_ui"`
	WebDAVService bool `json:"webdav_service"`
	OverallStatus bool `json:"overall_status"`
}

// healthTimeout bounds all probes together.
const healthTimeout = 30 * time.Second

func main() {
	if !healthy() {
		os.Exit(1)
	}
}

// healthy probes the qBittorrent API, web UI, and WebDAV endpoints.
func healthy() bool {
	var (
		configPath string
		debug      bool
	)
	flag.StringVar(&configPath, "config", "/data", "path to the data folder")
	flag.BoolVar(&debug, "debug", false, "enable debug mode for detailed output")
	flag.Parse()
	// Read-only: probing a container must never create or rewrite its
	// config.json or auth.json.
	cfg, err := config.LoadReadOnly(configPath, os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return false
	}
	// GetReader port from environment variable or use default
	port := cmp.Or(os.Getenv("QBIT_PORT"), cfg.Port)

	// Initialize status
	status := HealthStatus{
		QbitAPI:       false,
		WebUI:         false,
		WebDAVService: false,
		OverallStatus: false,
	}

	// Create a context with timeout for all HTTP requests
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	baseURL := cmp.Or(cfg.URLBase, "/")
	auth := cfg.GetAuth()

	status.QbitAPI = checkQbitAPI(ctx, client, baseURL, port, auth, cfg.UseAuth)
	status.WebUI = checkWebUI(ctx, client, baseURL, port, auth, cfg.UseAuth)
	status.WebDAVService = checkBaseWebdav(ctx, client, baseURL, port, cfg)
	// Determine overall status
	// Consider the application healthy if core services are running
	status.OverallStatus = status.QbitAPI && status.WebUI && status.WebDAVService

	// Optional: output health status as JSON for logging
	if debug {
		statusJSON, _ := json.MarshalIndent(status, "", "  ")
		_, _ = fmt.Fprintln(os.Stdout, string(statusJSON))
	}

	return status.OverallStatus
}

func checkQbitAPI(
	ctx context.Context,
	client *http.Client,
	baseURL, port string,
	auth *config.Auth,
	authMayBeRequired bool,
) bool {
	url := localURL(port, baseURL, "api/v2/app/version")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	addBearerAuth(req, auth)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer drainAndClose(resp)

	return isHealthyStatus(resp.StatusCode, authMayBeRequired, http.StatusOK)
}

func checkWebUI(
	ctx context.Context,
	client *http.Client,
	baseURL, port string,
	auth *config.Auth,
	authMayBeRequired bool,
) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localURL(port, baseURL, "version"), nil)
	if err != nil {
		return false
	}
	addBearerAuth(req, auth)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer drainAndClose(resp)

	return isHealthyStatus(resp.StatusCode, authMayBeRequired, http.StatusOK) || isRedirect(resp.StatusCode)
}

func checkBaseWebdav(ctx context.Context, client *http.Client, baseURL, port string, cfg *config.Config) bool {
	url := localURL(port, baseURL, "webdav/")
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", url, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer drainAndClose(resp)

	authMayBeRequired := cfg.UseAuth && cfg.EnableWebdavAuth
	return isHealthyStatus(
		resp.StatusCode,
		authMayBeRequired,
		http.StatusOK,
		http.StatusCreated,
		http.StatusMultiStatus,
	)
}

func localURL(port, baseURL, endpoint string) string {
	base := strings.Trim(baseURL, "/")
	endpoint = strings.TrimLeft(endpoint, "/")

	switch {
	case base == "" && endpoint == "":
		return fmt.Sprintf("http://localhost:%s/", port)
	case base == "":
		return fmt.Sprintf("http://localhost:%s/%s", port, endpoint)
	case endpoint == "":
		return fmt.Sprintf("http://localhost:%s/%s/", port, base)
	default:
		return fmt.Sprintf("http://localhost:%s/%s/%s", port, base, endpoint)
	}
}

func addBearerAuth(req *http.Request, auth *config.Auth) {
	if auth == nil || auth.APIToken == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+auth.APIToken)
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func isHealthyStatus(statusCode int, authMayBeRequired bool, expectedStatusCodes ...int) bool {
	if slices.Contains(expectedStatusCodes, statusCode) {
		return true
	}

	if !authMayBeRequired {
		return false
	}

	return statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden
}

func isRedirect(statusCode int) bool {
	return statusCode >= http.StatusMultipleChoices && statusCode < http.StatusBadRequest
}
