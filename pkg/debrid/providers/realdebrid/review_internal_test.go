package realdebrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func testRealDebrid(host string) *RealDebrid {
	return &RealDebrid{
		Host:    host,
		client:  request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config:  config.Debrid{Name: "realdebrid"},
		logger:  zerolog.Nop(),
		options: mkvOnly(),
	}
}

func TestGetTorrentsPaginatesOnRawPageSize(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "":
			_, _ = fmt.Fprint(w, `[{"id":"a","status":"downloading"},{"id":"b","status":"downloaded"}]`)
		case "2":
			_, _ = fmt.Fprint(w, `[{"id":"c","status":"downloaded"}]`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	torrents, err := testRealDebrid(server.URL).GetTorrents()
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 2 || torrents[0].ID != "b" || torrents[1].ID != "c" {
		t.Fatalf("GetTorrents() returned %d torrents, want b and c", len(torrents))
	}
}

// A 509 on file selection must still return the torrent so callers delete it.
func TestCheckStatusReturnsTorrentOnSlotLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(statusTooManyActive)
			return
		}
		_, _ = fmt.Fprint(
			w,
			`{"id":"t","status":"waiting_files_selection","files":[{"id":1,"path":"/a.mkv","bytes":1}]}`,
		)
	}))
	defer server.Close()
	torrent, err := testRealDebrid(server.URL).CheckStatus(&types.Torrent{ID: "t"})
	if !errors.Is(err, customerror.ErrTooManyActiveDownloads) {
		t.Fatalf("CheckStatus() error = %v, want too many active downloads", err)
	}
	if torrent == nil || torrent.ID != "t" {
		t.Fatalf("CheckStatus() torrent = %v, want the submitted torrent", torrent)
	}
}

// RD answers 404 for a torrent it dropped; that must read as gone so the grab is failed, not as an opaque API error.
func TestCheckStatusReportsDroppedTorrentAsNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	_, err := testRealDebrid(server.URL).CheckStatus(&types.Torrent{ID: "L2DZNJEZU2RZU", Name: "release"})
	if !errors.Is(err, customerror.ErrTorrentNotFound) {
		t.Fatalf("CheckStatus() error = %v, want ErrTorrentNotFound", err)
	}
}
