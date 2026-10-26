package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// failWriter fails the test with every line the server logs, so a template error reaches the test output.
type failWriter struct{ t *testing.T }

func (w failWriter) Write(p []byte) (int, error) {
	w.t.Errorf("server logged: %s", p)
	return len(p), nil
}

type fixturePage struct {
	name   string
	handle func(s *Server) http.HandlerFunc
}

// renderFixtures renders every page, in the store states that reach it, under urlBase, and the settings page's
// GET /api/config response for the last state.
func renderFixtures(t *testing.T, urlBase string) (map[string]string, []byte) {
	t.Helper()
	store := managertest.Store(t, func(cfg *config.Config) { cfg.URLBase = urlBase })
	s := &Server{config: store, urlBase: urlBase, logger: zerolog.New(failWriter{t}).Level(zerolog.WarnLevel)}
	s.loadUI(testStatic(t))
	pages := map[string]string{}
	render := func(page fixturePage) {
		w := httptest.NewRecorder()
		page.handle(s)(w, httptest.NewRequest(http.MethodGet, urlBase+page.name, nil))
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "</html>") {
			t.Fatalf("%s at %s: status %d, want 200 and a whole page:\n%s", page.name, urlBase, w.Code, body)
		}
		pages[page.name] = body
	}
	render(fixturePage{"setup", func(s *Server) http.HandlerFunc { return s.SetupHandler }})
	update(t, store, func(cfg *config.Config) error {
		cfg.DownloadFolder = "/downloads"
		cfg.Debrids = []config.Debrid{{Provider: "realdebrid", Name: "realdebrid", APIKey: "key"}}
		cfg.Arrs = []config.Arr{{Name: "sonarr", Host: "http://sonarr:8989", Token: "token"}}
		return nil
	})
	render(fixturePage{"register", func(s *Server) http.HandlerFunc { return s.RegisterHandler }})
	update(t, store, func(cfg *config.Config) error { return cfg.SetCredentials("admin", "password") })
	for _, page := range []fixturePage{
		{"login", func(s *Server) http.HandlerFunc { return s.LoginHandler }},
		{"index", func(s *Server) http.HandlerFunc { return s.IndexHandler }},
		{"download", func(s *Server) http.HandlerFunc { return s.DownloadHandler }},
		{"repair", func(s *Server) http.HandlerFunc { return s.RepairHandler }},
		{"reacquire", func(s *Server) http.HandlerFunc { return s.ReacquireHandler }},
		{"config", func(s *Server) http.HandlerFunc { return s.ConfigHandler }},
		{"stats", func(s *Server) http.HandlerFunc { return s.StatsHandler }},
		{"browse", func(s *Server) http.HandlerFunc { return s.BrowseHandler }},
	} {
		render(page)
	}
	cfg := *store.Get()
	response, err := json.Marshal(configResponse(cfg, cfg.Arrs))
	if err != nil {
		t.Fatal(err)
	}
	update(t, store, func(cfg *config.Config) error {
		cfg.Debrids = append(cfg.Debrids, config.Debrid{Provider: "torbox", Name: "torbox", APIKey: "key"})
		cfg.AlwaysRmTrackerUrls = true
		return nil
	})
	render(fixturePage{"download-options", func(s *Server) http.HandlerFunc { return s.DownloadHandler }})
	return pages, response
}

func update(t *testing.T, store *config.Store, edit func(*config.Config) error) {
	t.Helper()
	if _, err := store.Update(edit); err != nil {
		t.Fatal(err)
	}
}

// writeFixture writes body as name in dir, if dir is set.
func writeFixture(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRenderUIFixtures renders each page from the real templates; with UI_FIXTURES_DIR set it writes them for Vitest.
func TestRenderUIFixtures(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("UI_FIXTURES_DIR")
	if dir != "" && !filepath.IsAbs(dir) {
		t.Fatalf("UI_FIXTURES_DIR must be absolute, got %q", dir)
	}
	for suffix, urlBase := range map[string]string{"": "/", "@base": "/decypharr/"} {
		pages, response := renderFixtures(t, urlBase)
		for name, html := range pages {
			writeFixture(t, dir, name+suffix+".html", []byte(html))
		}
		writeFixture(t, dir, "config"+suffix+".json", response)
	}
	for name, body := range configSampleFiles(t) {
		writeFixture(t, dir, name, body)
	}
}
