package utils_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
)

func TestConstructMagnetKeepsReadableName(t *testing.T) {
	t.Parallel()
	m := utils.ConstructMagnet("abc", " My Movie [2020] ")
	if m.Name != "My Movie [2020]" {
		t.Errorf("Name = %q, want the unescaped display name", m.Name)
	}
	if want := "magnet:?xt=urn:btih:abc&dn=My+Movie+%5B2020%5D"; m.Link != want {
		t.Errorf("Link = %q, want %q", m.Link, want)
	}
}

func TestJoinURLKeepsWholeQueryAndCallerSlice(t *testing.T) {
	t.Parallel()
	paths := []string{"api", "search?q=a?b&x=1"}
	got, err := utils.JoinURL("http://h/", paths...)
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://h/api/search?q=a?b&x=1"; got != want {
		t.Errorf("JoinURL = %q, want %q", got, want)
	}
	if paths[1] != "search?q=a?b&x=1" {
		t.Errorf("caller slice rewritten to %q", paths[1])
	}
	if _, err = utils.JoinURL("http://h/"); err != nil {
		t.Errorf("JoinURL without paths: %v", err)
	}
}

func TestOpenMagnetHTTPURLReportsHTTPStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := utils.OpenMagnetHTTPURL(utils.NewDownloadClient(nil), server.URL, false)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404 status instead of a bencode parse error", err)
	}
	if _, _, err = utils.DownloadFile(
		utils.NewDownloadClient(nil),
		server.URL,
	); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("DownloadFile err = %v, want 404", err)
	}
}

// A server suggests a file name, never a path: DownloadFile keeps only the
// last element of whatever Content-Disposition or the URL names.
func TestDownloadFileNameIsOneElement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		disposition, urlPath, want string
	}{
		{`attachment; filename="Show.S01E01.nzb"`, "/get", "Show.S01E01.nzb"},
		{`attachment; filename="../../etc/cron.d/job.nzb"`, "/get", "job.nzb"},
		{`attachment; filename*=UTF-8''..%2F..%2Fescape.nzb`, "/get", "escape.nzb"},
		{`attachment; filename="..\\..\\win.nzb"`, "/get", "win.nzb"},
		{`attachment; filename=".."`, "/get", "downloaded_file"},
		{"", "/nzb/..%2Fup.nzb", "up.nzb"},
		{"", "/", "downloaded_file"},
	}
	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tt.disposition != "" {
				w.Header().Set("Content-Disposition", tt.disposition)
			}
			_, _ = w.Write([]byte("nzb"))
		}))
		name, _, err := utils.DownloadFile(utils.NewDownloadClient(nil), server.URL+tt.urlPath)
		server.Close()
		if err != nil {
			t.Fatalf("%q: %v", tt.disposition, err)
		}
		if name != tt.want {
			t.Errorf("Content-Disposition %q, path %q: name = %q, want %q", tt.disposition, tt.urlPath, name, tt.want)
		}
	}
}

// Indexers often answer a torrent URL with a redirect to a magnet link; that
// link is the result, not an unsupported-scheme error.
func TestOpenMagnetHTTPURLFollowsMagnetRedirect(t *testing.T) {
	t.Parallel()
	const magnet = "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=Example"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			http.Redirect(w, r, "/indexer", http.StatusFound)
			return
		}
		w.Header().Set("Location", magnet)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	got, err := utils.OpenMagnetHTTPURL(utils.NewDownloadClient(nil), server.URL+"/download", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.InfoHash != "8a19577fb5f690970ca43a57ff1011ae202244b8" || got.Name != "Example" {
		t.Fatalf("magnet = %+v, want the redirect target", got)
	}
}
