package torbox

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// A link TorBox refuses for good stops a read at once; a status that may pass does not.
func TestDownloadLinkErrorsAreTyped(t *testing.T) {
	t.Parallel()
	for status, permanent := range map[int]bool{http.StatusNotFound: true, http.StatusForbidden: true, http.StatusBadRequest: false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		tb := testTorbox(server.URL)
		tb.accountsManager = account.NewManager(
			config.Debrid{Name: "torbox", DownloadAPIKeys: []string{"download-token"}},
			types.ProviderOptions{Logger: tb.logger},
			nil,
		)
		_, err := tb.GetDownloadLink(t.Context(), "17", &types.File{ID: "1", Link: "torbox://17/1"})
		server.Close()
		var typed *customerror.Error
		if !errors.As(err, &typed) || typed.IsPermanent() != permanent {
			t.Errorf("status %d: error = %v, want a typed error with permanent = %v", status, err, permanent)
		}
	}
}
