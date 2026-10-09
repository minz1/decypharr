package debridlink

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestTorrentResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		body      string
		files     int
		wantError bool
	}{
		{"accepted file", `{"success":true,"value":[{"id":"torrent","hashString":"hash","status":100,"files":[{"id":"file","name":"movie.mkv","size":1000,"downloadUrl":"https://files.example/movie"}]}]}`, 1, false},
		{"filtered file", `{"success":true,"value":[{"id":"torrent","status":100,"files":[{"id":"file","name":"note.txt","size":1000}]}]}`, 0, false},
		{"no files", `{"success":true,"value":[{"id":"torrent","status":100,"files":[]}]}`, 0, false},
		{"missing value", `{"success":true}`, 0, true},
		{"failed response", `{"success":false,"value":[]}`, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, test.body) }),
			)
			defer server.Close()
			provider, err := New(config.Debrid{Name: "debridlink", APIKey: "token"}, nil, mkvOnly())
			if err != nil {
				t.Fatal(err)
			}
			provider.Host = server.URL
			for _, list := range []bool{false, true} {
				torrent, fetchErr := fetchTorrent(provider, list)
				if test.wantError {
					if fetchErr == nil {
						t.Fatalf("list=%v: invalid envelope accepted", list)
					}
					continue
				}
				if fetchErr != nil {
					t.Fatal(fetchErr)
				}
				checkTorrent(t, list, torrent, test.files)
			}
		})
	}
}

// fetchTorrent reads torrent "torrent" through the list or the info endpoint.
func fetchTorrent(provider *DebridLink, list bool) (*types.Torrent, error) {
	if !list {
		return provider.GetTorrent("torrent")
	}
	torrents, _, err := provider.getTorrents(1, pageSize)
	if err != nil || len(torrents) != 1 {
		return nil, err
	}
	return torrents[0], nil
}

// checkTorrent asserts a downloaded torrent with files allowed files.
func checkTorrent(t *testing.T, list bool, torrent *types.Torrent, files int) {
	t.Helper()
	if torrent == nil || torrent.Files == nil || len(torrent.Files) != files ||
		torrent.Status != types.TorrentStatusDownloaded {
		t.Fatalf("list=%v: torrent=%#v", list, torrent)
	}
	if files == 0 {
		return
	}
	file := torrent.Files["movie.mkv"]
	if file.ID != "file" || file.Link != "https://files.example/movie" || torrent.InfoHash != "hash" {
		t.Fatalf("lost file identity: %#v", torrent)
	}
}

// mkvOnly allows only .mkv files, the way an operator's allowed_file_types
// would.
func mkvOnly() types.ProviderOptions {
	cfg := &config.Config{AllowedExt: []string{"mkv"}}
	return types.ProviderOptions{ValidateFile: cfg.ValidateFileAllowed, Logger: zerolog.Nop()}
}

func TestGetTorrentReportsGoneAsNotFound(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		status int
		body   string
		gone   bool
	}{
		"404":          {http.StatusNotFound, ``, true},
		"empty list":   {http.StatusOK, `{"success":true,"value":[]}`, true},
		"server error": {http.StatusInternalServerError, ``, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			provider, err := New(config.Debrid{Name: "debridlink", APIKey: "token"}, nil, mkvOnly())
			if err != nil {
				t.Fatal(err)
			}
			provider.Host = server.URL
			_, err = provider.GetTorrent("torrent")
			if got := errors.Is(err, customerror.ErrTorrentNotFound); got != test.gone || err == nil {
				t.Fatalf("err=%v, want not-found=%v", err, test.gone)
			}
		})
	}
}
