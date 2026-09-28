package sabnzbd_test

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/server/sabnzbd"
)

func newTestRoutes(t *testing.T) http.Handler {
	t.Helper()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	cfg := config.Get()
	cfg.UseAuth = true
	cfg.Auth = &config.Auth{APIToken: "test-token", TokenOnly: true}
	cfg.Usenet.Providers = []config.UsenetProvider{
		{Host: "news.example.test", Port: 563, Username: "user", Password: "provider-secret"},
	}
	mgr := manager.New()
	t.Cleanup(func() { _ = mgr.Stop() })
	return sabnzbd.New(mgr).Routes()
}

//nolint:paralleltest // mutates the process-wide config singleton
func TestGetConfigDoesNotExposeProviderPasswords(t *testing.T) {
	routes := newTestRoutes(t)
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/?mode=get_config&ma_password=test-token", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "news.example.test") {
		t.Fatalf("server list missing from config: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "provider-secret") {
		t.Fatal("get_config returned the usenet provider password")
	}
}

//nolint:paralleltest // mutates the process-wide config singleton
func TestOversizedBodyIsRejectedBeforeAuth(t *testing.T) {
	routes := newTestRoutes(t)
	// Multipart file parts spill to temp files with no size limit of their
	// own, so only the body cap bounds them. Stream the body to keep the
	// test's own memory flat.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("mode", "queue")
		_ = mw.WriteField("ma_password", "test-token")
		part, _ := mw.CreateFormFile("name", "big.nzb")
		_, _ = io.CopyN(part, zeros{}, 257<<20)
		_ = mw.Close()
		_ = pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPost, "/api/", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, req)
	_ = pr.Close()
	if w.Code == http.StatusOK {
		t.Fatal("a body over the cap was accepted")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
