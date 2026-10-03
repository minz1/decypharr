package debridlink

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Pagination must follow the provider's raw page size, not the filtered result.
func TestPaginationIgnoresFilteredEntries(t *testing.T) {
	t.Parallel()
	const perPage = 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch {
		case r.URL.Path == "/seedbox/list" && page == "0":
			// A page of unfinished torrents only.
			_, _ = fmt.Fprint(w, `{"success":true,"value":[{"id":"a","status":50}]}`)
		case r.URL.Path == "/seedbox/list" && page == "1":
			_, _ = fmt.Fprint(w, `{"success":true,"value":[{"id":"b","status":100}]}`)
		case r.URL.Path == "/downloader/list" && page == "0":
			// A full page whose first entry is expired.
			entries := make([]string, perPage)
			entries[0] = `{"id":"old","created":1}`
			for i := 1; i < perPage; i++ {
				entries[i] = fmt.Sprintf(`{"id":"l%d","created":%d}`, i, time.Now().Unix())
			}
			_, _ = fmt.Fprintf(w, `{"success":true,"value":[%s]}`, strings.Join(entries, ","))
		case r.URL.Path == "/downloader/list" && page == "1":
			_, _ = fmt.Fprintf(w, `{"success":true,"value":[{"id":"last","created":%d}]}`, time.Now().Unix())
		default:
			_, _ = fmt.Fprint(w, `{"success":true,"value":[]}`)
		}
	}))
	defer server.Close()
	provider, err := New(
		config.Debrid{Name: "debridlink", APIKey: "token", DownloadAPIKeys: []string{"token"}},
		nil,
		types.ProviderOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	provider.Host = server.URL

	torrents, err := provider.GetTorrents()
	if err != nil || len(torrents) != 1 || torrents[0].ID != "b" {
		t.Fatalf("GetTorrents() = %d torrents, %v; want torrent b from page 1", len(torrents), err)
	}
	links, err := provider.fetchDownloadLinks(provider.accountsManager.Current())
	if err != nil || len(links) != perPage {
		t.Fatalf("fetchDownloadLinks() = %d links, %v; want %d", len(links), err, perPage)
	}
}
