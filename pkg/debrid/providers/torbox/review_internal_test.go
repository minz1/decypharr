package torbox

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

//nolint:paralleltest // config.SetConfigPath mutates the process-wide config.
func TestFetchDownloadLinkReportsProviderErrors(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "download-token" {
			t.Errorf("token = %q", r.URL.Query().Get("token"))
		}
		if fail.Load() {
			_, _ = fmt.Fprint(w, `{"success":false,"error":"DATABASE_ERROR","data":null}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"success":true,"data":"https://cdn.example.test/file"}`)
	}))
	defer server.Close()
	tb := testTorbox(server.URL)
	tb.accountsManager = account.NewManager(
		config.Debrid{Name: "torbox", DownloadAPIKeys: []string{"download-token"}},
		nil,
		tb.logger,
	)
	file := &types.File{Id: "1", Link: "torbox://17/1"}
	if _, err := tb.GetDownloadLink(t.Context(), "17", file); err == nil {
		t.Fatal("provider error returned no error")
	}
	fail.Store(false)
	dl, err := tb.GetDownloadLink(t.Context(), "17", file)
	if err != nil {
		t.Fatalf("GetDownloadLink() error = %v", err)
	}
	if dl.DownloadLink != "https://cdn.example.test/file" || dl.Token != "download-token" {
		t.Fatalf("link = %q token = %q", dl.DownloadLink, dl.Token)
	}
}

func TestUpdateTorrentRejectsNullData(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":false,"error":"NOT_FOUND","data":null}`)
	}))
	defer server.Close()
	if err := testTorbox(server.URL).UpdateTorrent(&types.Torrent{Id: "17"}); err == nil {
		t.Fatal("UpdateTorrent() with null data returned no error")
	}
}

func TestCheckFileRefreshesStalePresence(t *testing.T) {
	t.Parallel()
	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "0" {
			_, _ = fmt.Fprint(w, `{"success":true,"data":[]}`)
			return
		}
		if loads.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"success":true,"data":[{"id":1,"download_present":true}]}`)
			return
		}
		_, _ = fmt.Fprint(
			w,
			`{"success":true,"data":[{"id":1,"download_present":true},{"id":2,"download_present":true}]}`,
		)
	}))
	defer server.Close()
	tb := testTorbox(server.URL)
	if err := tb.CheckFile(t.Context(), "", "torbox://2/1"); !errors.Is(err, customerror.HosterUnavailableError) {
		t.Fatalf("unknown torrent: %v, want hoster unavailable", err)
	}
	tb.downloadPresentAt = time.Now().Add(-2 * downloadPresentTTL)
	if err := tb.CheckFile(t.Context(), "", "torbox://2/1"); err != nil {
		t.Fatalf("torrent added after first load: %v, want present", err)
	}
}
