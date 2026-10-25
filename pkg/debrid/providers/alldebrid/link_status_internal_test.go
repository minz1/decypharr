package alldebrid

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// A link AllDebrid refuses for good stops a read at once; a status that may pass does not.
func TestDownloadLinkErrorsAreTyped(t *testing.T) {
	t.Parallel()
	for status, permanent := range map[int]bool{http.StatusNotFound: true, http.StatusUnauthorized: true, http.StatusBadRequest: false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		ad := &AllDebrid{
			Host:   server.URL,
			config: config.Debrid{Name: "alldebrid"},
			accountsManager: account.NewManager(
				config.Debrid{Name: "alldebrid", DownloadAPIKeys: []string{"download-key"}},
				types.ProviderOptions{Logger: zerolog.Nop()},
				nil,
			),
		}
		_, err := ad.GetDownloadLink(t.Context(), "1", &types.File{Link: "https://alldebrid.example/f"})
		server.Close()
		var typed *customerror.Error
		if !errors.As(err, &typed) || typed.IsPermanent() != permanent {
			t.Errorf("status %d: error = %v, want a typed error with permanent = %v", status, err, permanent)
		}
	}
}
