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

	_, err := utils.OpenMagnetHTTPURL(server.URL, false)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404 status instead of a bencode parse error", err)
	}
	if _, _, err = utils.DownloadFile(server.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("DownloadFile err = %v, want 404", err)
	}
}
