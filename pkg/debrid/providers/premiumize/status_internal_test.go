package premiumize

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type finalizingCase struct {
	name          string
	transfer      string // raw Premiumize transfer status
	linkedAfter   int    // folder lists without a link before it appears (-1: never)
	allowUncached bool
	wantStatus    types.TorrentStatus
	wantErr       error
	notErr        error
	wantLists     int32
}

// A finished transfer whose folder tree has no links yet is the provider still
// finalising, not an uncached release. Reporting it as not cached makes the
// manager delete the transfer and tell hearsay the release is uncached.
func TestCheckStatusFinalizingTransferIsNotUncached(t *testing.T) {
	t.Parallel()
	for _, tc := range []finalizingCase{
		{
			name: "finished without links then ready", transfer: "finished", linkedAfter: 2,
			wantStatus: types.TorrentStatusDownloaded, wantLists: 3,
		},
		{
			name: "finished and ready at once", transfer: "finished",
			wantStatus: types.TorrentStatusDownloaded, wantLists: 1,
		},
		{
			name: "finished without links forever hits the cap", transfer: "finished", linkedAfter: -1,
			wantStatus: types.TorrentStatusDownloading, wantLists: 4,
			wantErr: customerror.ErrProviderStatusTimeout, notErr: customerror.ErrTorrentNotCached,
		},
		{
			name: "running is uncached", transfer: "running",
			wantStatus: types.TorrentStatusDownloading, wantLists: 1, wantErr: customerror.ErrTorrentNotCached,
		},
		{
			name: "queued is uncached", transfer: "queued",
			wantStatus: types.TorrentStatusQueued, wantLists: 1, wantErr: customerror.ErrTorrentNotCached,
		},
		{
			name: "running allowed when uncached downloads are on", transfer: "running", allowUncached: true,
			wantStatus: types.TorrentStatusDownloading, wantLists: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func (tc finalizingCase) run(t *testing.T) {
	var lists, deletes atomic.Int32
	server := httptest.NewServer(tc.mux(&lists, &deletes))
	t.Cleanup(server.Close)

	pm := &Premiumize{
		Host:               server.URL,
		client:             request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config:             config.Debrid{Name: "premiumize"},
		logger:             zerolog.Nop(),
		options:            types.ProviderOptions{ValidateFile: func(string, int64) error { return nil }},
		pollInterval:       time.Millisecond,
		maxFinalizingPolls: 3,
	}
	result, err := pm.CheckStatus(t.Context(), &types.Torrent{ID: "t1", DownloadUncached: tc.allowUncached})
	switch {
	case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
		t.Fatalf("error = %v, want %v", err, tc.wantErr)
	case tc.wantErr == nil && err != nil:
		t.Fatal(err)
	case tc.notErr != nil && errors.Is(err, tc.notErr):
		t.Fatalf("error = %v must not be %v", err, tc.notErr)
	}
	if result == nil || result.Status != tc.wantStatus {
		t.Fatalf("result = %#v, want status %s", result, tc.wantStatus)
	}
	if lists.Load() != tc.wantLists {
		t.Fatalf("transfer lists = %d, want %d", lists.Load(), tc.wantLists)
	}
	if deletes.Load() != 0 {
		t.Fatalf("provider deleted the transfer %d times", deletes.Load())
	}
}

func (tc finalizingCase) mux(lists, deletes *atomic.Int32) *http.ServeMux {
	var folderLists atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/transfer/list", func(w http.ResponseWriter, _ *http.Request) {
		lists.Add(1)
		_, _ = fmt.Fprintf(
			w,
			`{"status":"success","transfers":[{"id":"t1","name":"Release","status":%q,"progress":1,`+
				`"folder_id":"f1","file_id":null}]}`,
			tc.transfer,
		)
	})
	mux.HandleFunc("GET /api/folder/list", func(w http.ResponseWriter, _ *http.Request) {
		link := ""
		if tc.linkedAfter >= 0 && int(folderLists.Add(1)) > tc.linkedAfter {
			link = "https://example.test/movie.mkv"
		}
		_, _ = fmt.Fprintf(
			w,
			`{"status":"success","folder_id":"f1","content":[{"id":"file-1","name":"movie.mkv","type":"file",`+
				`"size":1024,"link":%q}]}`,
			link,
		)
	})
	mux.HandleFunc("POST /api/transfer/delete", func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		_, _ = fmt.Fprint(w, `{"status":"success"}`)
	})
	return mux
}
