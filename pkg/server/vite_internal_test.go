package server

import (
	"compress/gzip"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// testStatic stands in for assets/build, so tests don't depend on whether the UI was built.
func testStatic(t *testing.T) fstest.MapFS {
	t.Helper()
	manifest, err := os.ReadFile("testdata/vite/.vite/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"vite/.vite/manifest.json":    {Data: manifest},
		"vite/assets/styles-AAAA.css": {Data: []byte(strings.Repeat("body { color: red; }\n", 20))},
		"vite/assets/login-BBBB.js":   {Data: []byte(strings.Repeat("console.log('login');\n", 20))},
		"js/common.js":                {Data: []byte(strings.Repeat("console.log('common');\n", 20))},
	}
}

func fixtureManifest(t *testing.T) viteManifest {
	t.Helper()
	m, err := loadViteManifest(os.DirFS("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func renderVite(t *testing.T, m viteManifest, urlBase, action string) (string, error) {
	t.Helper()
	tmpl := template.Must(parseTemplates(m.funcs(urlBase)).New("probe").Parse(`{{template "vite" (` + action + `)}}`))
	var b strings.Builder
	err := tmpl.Execute(&b, nil)
	return b.String(), err
}

func TestEntryTagsOrder(t *testing.T) {
	t.Parallel()
	got, err := renderVite(t, fixtureManifest(t), "/x/", `viteEntry "pkg/server/ui/pages/login.ts"`)
	if err != nil {
		t.Fatal(err)
	}
	want := `<link rel="stylesheet" href="/x/assets/vite/assets/login-EEEE.css">` +
		`<link rel="stylesheet" href="/x/assets/vite/assets/shared-FFFF.css">` +
		`<script type="module" src="/x/assets/vite/assets/login-BBBB.js"></script>` +
		`<link rel="modulepreload" href="/x/assets/vite/assets/shared-CCCC.js">` +
		`<link rel="modulepreload" href="/x/assets/vite/assets/other-DDDD.js">`
	if got != want {
		t.Fatalf("entryTags:\n got %s\nwant %s", got, want)
	}
}

func TestCSSTags(t *testing.T) {
	t.Parallel()
	m := fixtureManifest(t)
	got, err := renderVite(t, m, "/", `viteCSS "pkg/server/ui/styles.css"`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `<link rel="stylesheet" href="/assets/vite/assets/styles-AAAA.css">`; got != want {
		t.Fatalf("viteCSS = %s, want %s", got, want)
	}
	if _, cssErr := m.cssTags("/", "pkg/server/ui/pages/login.ts"); cssErr == nil {
		t.Fatal("cssTags accepted a script entry")
	}
	if _, entryErr := m.entryTags("/", "pkg/server/ui/styles.css"); entryErr == nil {
		t.Fatal("entryTags accepted a CSS entry")
	}
}

func TestUnknownEntryIsAnError(t *testing.T) {
	t.Parallel()
	_, err := renderVite(t, fixtureManifest(t), "/", `viteEntry "nope.ts"`)
	if err == nil || !strings.Contains(err.Error(), "nope.ts") {
		t.Fatalf("err = %v, want one naming nope.ts", err)
	}
}

func TestMissingManifestIsNotBuilt(t *testing.T) {
	t.Parallel()
	_, err := loadViteManifest(fstest.MapFS{".placeholder": {}})
	if !errors.Is(err, errUINotBuilt) {
		t.Fatalf("err = %v, want errUINotBuilt", err)
	}
}

func TestTemplatesNameKnownEntries(t *testing.T) {
	t.Parallel()
	m, err := loadViteManifest(embeddedStatic())
	if errors.Is(err, errUINotBuilt) {
		t.Skip("UI not built; the nix build runs this against the real manifest")
	}
	if err != nil {
		t.Fatal(err)
	}
	ref := regexp.MustCompile(`(viteEntry|viteCSS) "([^"]+)"`)
	entries, err := fs.ReadDir(content, "templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		src, readErr := fs.ReadFile(content, "templates/"+e.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, match := range ref.FindAllStringSubmatch(string(src), -1) {
			if _, ok := m[match[2]]; !ok {
				t.Errorf("%s: %s %q is not in the Vite manifest", e.Name(), match[1], match[2])
			}
		}
	}
}

func TestViteFilesAreImmutable(t *testing.T) {
	t.Parallel()
	js := strings.Repeat("console.log('decypharr');\n", 20)
	s := newTestServer(t, managertest.Store(t, nil))
	s.static = fstest.MapFS{
		"vite/assets/login-BBBB.js": {Data: []byte(js)},
		"vite/.vite/manifest.json":  {Data: []byte(`{}`)},
	}
	s.assets = loadAssetManifest(s.static)
	h := s.WebRoutes()

	w := getAsset(h, "/assets/vite/assets/login-BBBB.js", map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("status = %d, Cache-Control = %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", w.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != js {
		t.Fatal("gunzipped body differs from the file")
	}

	missing := getAsset(h, "/assets/vite/assets/gone-XXXX.js", nil)
	if missing.Code != http.StatusNotFound || strings.Contains(missing.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("missing file: status = %d, Cache-Control = %q", missing.Code, missing.Header().Get("Cache-Control"))
	}
	if m := getAsset(h, "/assets/vite/.vite/manifest.json", nil); m.Code != http.StatusNotFound {
		t.Fatalf("manifest status = %d, want 404", m.Code)
	}
}

func TestPageWithoutBuildSaysSo(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, managertest.Store(t, nil))
	s.loadUI(fstest.MapFS{".placeholder": {}})
	h := s.WebRoutes()
	w := getAsset(h, "/setup", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "UI assets not built") {
		t.Fatalf("/setup: status %d, body %q", w.Code, w.Body.String())
	}
	if v := getAsset(h, "/version", nil); v.Code != http.StatusOK {
		t.Fatalf("/version: status %d", v.Code)
	}
}

func TestLayoutsLinkViteCSS(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, managertest.Store(t, nil))
	for _, layout := range []string{"layout", "setup_layout"} {
		w := httptest.NewRecorder()
		s.renderPage(w, layout, "index", "Queues", nil)
		page := w.Body.String()
		if !strings.Contains(page, `<link rel="stylesheet" href="/assets/vite/assets/styles-AAAA.css">`) {
			t.Errorf("%s does not link the Vite stylesheet:\n%s", layout, page)
		}
		if strings.Contains(page, "assets/css/styles.css") {
			t.Errorf("%s still links the legacy stylesheet", layout)
		}
	}
}
