package sabnzbd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func newCategoryRouter(t *testing.T) http.Handler {
	t.Helper()
	mgr, store := managertest.New(t, func(c *config.Config) {
		c.UseAuth = true
		c.Auth = &config.Auth{APIToken: "test-token", TokenOnly: true}
		c.DownloadFolder = t.TempDir()
		c.Categories = []string{"sonarr", "bindery"}
		c.Arrs = []config.Arr{{Name: "sonarr", Host: "http://sonarr.example.test", Token: "arr-token"}}
	})
	return New(mgr, store, zerolog.Nop()).Routes()
}

func fetchMode(t *testing.T, routes http.Handler, mode string) []byte {
	t.Helper()
	values := url.Values{"mode": {mode}, "output": {"json"}, "ma_password": {"test-token"}}
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, sabRequest(http.MethodGet, values))
	if w.Code != http.StatusOK {
		t.Fatalf("%s status = %d: %s", mode, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

func TestGetCatsAnswersInSABnzbdsShape(t *testing.T) {
	t.Parallel()
	var got struct {
		Categories []string `json:"categories"`
	}
	body := fetchMode(t, newCategoryRouter(t), ModeGetCats)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("get_cats %s: %v", body, err)
	}
	if want := []string{"*", "sonarr", "bindery"}; !slices.Equal(got.Categories, want) {
		t.Fatalf("get_cats categories = %q, want %q", got.Categories, want)
	}
}

func TestGetConfigListsEachCategoryOnce(t *testing.T) {
	t.Parallel()
	var got struct {
		Config struct {
			Categories []struct {
				Name string `json:"name"`
			} `json:"categories"`
		} `json:"config"`
	}
	body := fetchMode(t, newCategoryRouter(t), ModeGetConfig)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("get_config %s: %v", body, err)
	}
	var names []string
	for _, c := range got.Config.Categories {
		names = append(names, c.Name)
	}
	if want := []string{"sonarr", "bindery"}; !slices.Equal(names, want) {
		t.Fatalf("get_config categories = %q, want %q", names, want)
	}
}

func TestGetCatsListsTheDefaultCategoryOnce(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(c *config.Config) {
		c.UseAuth = true
		c.Auth = &config.Auth{APIToken: "test-token", TokenOnly: true}
		c.DownloadFolder = t.TempDir()
		c.Categories = []string{"*", "bindery"}
	})
	var got CategoriesResponse
	body := fetchMode(t, New(mgr, store, zerolog.Nop()).Routes(), ModeGetCats)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("get_cats %s: %v", body, err)
	}
	if want := []string{"*", "bindery"}; !slices.Equal(got.Categories, want) {
		t.Fatalf("get_cats categories = %q, want %q", got.Categories, want)
	}
}
