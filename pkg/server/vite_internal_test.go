package server

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// testStatic stands in for assets/build, so tests don't depend on whether the UI was built.
func testStatic(t *testing.T) fstest.MapFS {
	t.Helper()
	manifest := templateViteManifest(t)
	static := fstest.MapFS{
		"vite/.vite/manifest.json": {Data: manifest},
		"js/common.js":             {Data: []byte(strings.Repeat("console.log('common');\n", 20))},
	}
	var m viteManifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	for _, c := range m {
		static["vite/"+c.File] = &fstest.MapFile{Data: []byte(strings.Repeat("/* "+c.File+" */\n", 20))}
	}
	return static
}

const viteRefPattern = `(viteEntry|viteCSS) "([^"]+)"`

// templateViteRefs returns each viteEntry or viteCSS call in templates/*.html as {template, func, entry}.
func templateViteRefs(t *testing.T) [][3]string {
	t.Helper()
	ref := regexp.MustCompile(viteRefPattern)
	entries, err := fs.ReadDir(content, "templates")
	if err != nil {
		t.Fatal(err)
	}
	var refs [][3]string
	for _, e := range entries {
		src, readErr := fs.ReadFile(content, "templates/"+e.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, match := range ref.FindAllStringSubmatch(string(src), -1) {
			refs = append(refs, [3]string{e.Name(), match[1], match[2]})
		}
	}
	return refs
}

// templateViteManifest is a manifest with an entry for every name the templates pass to viteEntry or viteCSS.
func templateViteManifest(t *testing.T) []byte {
	t.Helper()
	type chunk struct {
		File    string `json:"file"`
		Src     string `json:"src"`
		IsEntry bool   `json:"isEntry"`
	}
	ext := map[string]string{"viteEntry": ".js", "viteCSS": ".css"}
	manifest := map[string]chunk{}
	for _, ref := range templateViteRefs(t) {
		name := ref[2]
		base := strings.TrimSuffix(path.Base(name), path.Ext(name))
		manifest[name] = chunk{File: "assets/" + base + "-TEST" + ext[ref[1]], Src: name, IsEntry: true}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
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
		t.Fatalf("viteEntry:\n got %s\nwant %s", got, want)
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
	if _, cssErr := m.tags("/", "pkg/server/ui/pages/login.ts", ".css"); cssErr == nil {
		t.Fatal("viteCSS accepted a script entry")
	}
	if _, entryErr := m.tags("/", "pkg/server/ui/styles.css", ".js"); entryErr == nil {
		t.Fatal("viteEntry accepted a CSS entry")
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
	for _, ref := range templateViteRefs(t) {
		if _, ok := m[ref[2]]; !ok {
			t.Errorf("%s: %s %q is not in the Vite manifest", ref[0], ref[1], ref[2])
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
	version := s.assets.version
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
	for _, target := range []string{"/assets/vite/.vite/manifest.json", "/assets/vite/.vite/", "/assets/vite/.vite"} {
		if m := getAsset(h, target, nil); m.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", target, m.Code)
		}
	}
	for _, dir := range []string{"/assets/vite/assets/", "/assets/vite/assets", "/assets/vite/", "/assets/js/?v=" + version} {
		d := getAsset(h, dir, nil)
		if strings.Contains(d.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("%s: directory cached as immutable (status %d)", dir, d.Code)
		}
	}
}

func TestPageWithoutBuildSaysSo(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, managertest.Store(t, nil))
	s.loadUI(fstest.MapFS{".placeholder": {}})
	h := s.WebRoutes()
	w := getAsset(h, "/setup", nil)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "UI assets not built") {
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
		if !strings.Contains(page, `<link rel="stylesheet" href="/assets/vite/assets/styles-TEST.css">`) {
			t.Errorf("%s does not link the Vite stylesheet:\n%s", layout, page)
		}
		if strings.Contains(page, "assets/css/styles.css") {
			t.Errorf("%s still links the legacy stylesheet", layout)
		}
	}
}

// A submit before the login module loads must not put the password in the URL.
func TestLoginFormPosts(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, managertest.Store(t, nil))
	w := httptest.NewRecorder()
	s.renderPage(w, "layout", "login", "Login", map[string]any{"TokenOnly": false})
	if !strings.Contains(w.Body.String(), `<form id="loginForm" method="post"`) {
		t.Fatal("the login form does not declare method=post")
	}
}
