package strm_test

import (
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/strm"
)

func TestFileURLRoundTrip(t *testing.T) {
	t.Parallel()
	base := "https://media.example.com/decypharr"
	infohash := "aabbccddeeff00112233445566778899aabbccdd"
	fileID := "0123456789abcdef"
	u := strm.FileURL(base, "secret", infohash, fileID, "Movie 2023 [x265].mkv")

	if !strings.HasPrefix(u, base+"/stream/"+infohash+"/"+fileID+"/") {
		t.Fatalf("unexpected url: %s", u)
	}
	ih, id, ok := strm.ParseURL(u)
	if !ok || ih != infohash || id != fileID {
		t.Fatalf("strm.ParseURL(%q) = %q, %q, %v", u, ih, id, ok)
	}
}

func TestParseURLRejectsForeign(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",
		"not a url",
		"plex://movie/xyz",
		"http://host:8282/webdav/stream/__all__/Some.Release/file.mkv", // legacy shape
		"http://host/stream/short/xyz/name.mkv",
		"file:///stream/aabbccddeeff00112233445566778899aabbccdd/0123456789abcdef/x.mkv",
	}
	for _, raw := range bad {
		if _, _, ok := strm.ParseURL(raw); ok {
			t.Errorf("strm.ParseURL(%q) unexpectedly ok", raw)
		}
	}
}

func TestSignVerify(t *testing.T) {
	t.Parallel()
	infohash := "aabbccddeeff00112233445566778899aabbccdd"
	fileID := "0123456789abcdef"
	sig := strm.Sign("secret", infohash, fileID)
	if !strm.Verify("secret", infohash, fileID, sig) {
		t.Fatal("valid signature rejected")
	}
	if strm.Verify("secret", infohash, fileID, "") {
		t.Fatal("empty signature accepted")
	}
	if strm.Verify("secret", infohash, fileID, sig+"00") {
		t.Fatal("tampered signature accepted")
	}
	if strm.Verify("other", infohash, fileID, sig) {
		t.Fatal("signature accepted with wrong secret")
	}
}

func TestBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{AppURL: "https://x.example.com/", URLBase: "/"}, "https://x.example.com"},
		{config.Config{AppURL: "https://x.example.com", URLBase: "/base/"}, "https://x.example.com/base"},
		{config.Config{AppURL: "https://x.example.com/base", URLBase: "/base/"}, "https://x.example.com/base"},
		{config.Config{BindAddress: "0.0.0.0", Port: "8282", URLBase: "/"}, "http://localhost:8282"},
		{config.Config{BindAddress: "10.0.0.5", Port: "9090"}, "http://10.0.0.5:9090"},
	}
	for _, tt := range tests {
		if got := strm.BaseURL(&tt.cfg); got != tt.want {
			t.Errorf("strm.BaseURL(%+v) = %q, want %q", tt.cfg, got, tt.want)
		}
	}
}

func TestFileName(t *testing.T) {
	t.Parallel()
	if got := strm.FileName("Movie.2023.1080p.mkv", false); got != "Movie.2023.1080p.strm" {
		t.Errorf("FileName replace = %q", got)
	}
	if got := strm.FileName("Movie.2023.1080p.mkv", true); got != "Movie.2023.1080p.mkv.strm" {
		t.Errorf("FileName keep = %q", got)
	}
	// Unknown extensions are not media extensions and must not be stripped.
	if got := strm.FileName("Show.S01E01.Part.1", false); got != "Show.S01E01.Part.1.strm" {
		t.Errorf("FileName no-ext = %q", got)
	}
}

func TestIsSidecar(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"movie.en.srt", "movie.SUB", "movie.nfo", "movie.idx"} {
		if !strm.IsSidecar(name) {
			t.Errorf("strm.IsSidecar(%q) = false", name)
		}
	}
	for _, name := range []string{"movie.mkv", "movie", "movie.rar"} {
		if strm.IsSidecar(name) {
			t.Errorf("strm.IsSidecar(%q) = true", name)
		}
	}
}
