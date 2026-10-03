package alldebrid

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Links must carry the token of the account that unlocked them, or the
// account manager cannot find the account to store or delete them.
func TestFetchedLinkUsesAccountToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"link":"https://cdn.example.test/f","id":"x"}}`)
	}))
	defer server.Close()
	ad := &AllDebrid{
		Host:   server.URL,
		APIKey: "main-key",
		config: config.Debrid{Name: "alldebrid"},
		accountsManager: account.NewManager(
			config.Debrid{Name: "alldebrid", DownloadAPIKeys: []string{"download-key"}},
			0,
			nil,
			zerolog.Nop(),
		),
	}
	dl, err := ad.GetDownloadLink(t.Context(), "1", &types.File{Link: "https://alldebrid.example/f"})
	if err != nil {
		t.Fatal(err)
	}
	if dl.Token != "download-key" {
		t.Fatalf("token = %q, want the fetching account's token", dl.Token)
	}
	if deleteErr := ad.accountsManager.DeleteDownloadLink(dl, nil); deleteErr != nil {
		t.Fatalf("DeleteDownloadLink() = %v", deleteErr)
	}
}
