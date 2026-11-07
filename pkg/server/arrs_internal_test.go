package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// GET /api/arrs is read by the repair page for names only; it must never hand
// an Arr's API token to the browser.
func TestGetArrsOmitsAPITokens(t *testing.T) {
	t.Parallel()
	mgr, _ := managertest.New(t, func(cfg *config.Config) {
		cfg.Arrs = []config.Arr{
			{
				Name: "sonarr", Host: "http://sonarr.invalid", Token: "sonarr-secret",
				SkipRepair: true, SelectedDebrid: "realdebrid", ImportWebhook: true,
			},
			{Name: "radarr", Host: "http://radarr.invalid", Token: "radarr-secret"},
		}
	})
	s := &Server{manager: mgr, logger: zerolog.Nop()}

	w := httptest.NewRecorder()
	s.handleGetArrs(w, httptest.NewRequest(http.MethodGet, "/api/arrs", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("response leaks a token: %s", w.Body.String())
	}
	var arrs []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &arrs); err != nil {
		t.Fatal(err)
	}
	if len(arrs) != 2 {
		t.Fatalf("got %d arrs, want 2: %s", len(arrs), w.Body.String())
	}
	for _, item := range arrs {
		if _, ok := item["token"]; ok {
			t.Errorf("%v has a token key", item["name"])
		}
		for _, key := range []string{"name", "host", "type", "skip_repair", "download_uncached"} {
			if _, ok := item[key]; !ok {
				t.Errorf("%v lost the %q key", item["name"], key)
			}
		}
	}
	sonarr := arrs[1]
	if sonarr["name"] != "sonarr" || sonarr["host"] != "http://sonarr.invalid" || sonarr["skip_repair"] != true ||
		sonarr["selected_debrid"] != "realdebrid" || sonarr["import_webhook"] != true {
		t.Errorf("sonarr = %v, want its configured fields kept", sonarr)
	}
}
