package premiumize

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// A link Premiumize refuses for good stops a read at once; a status that may pass does not.
func TestDownloadLinkErrorsAreTyped(t *testing.T) {
	t.Parallel()
	for status, permanent := range map[int]bool{http.StatusNotFound: true, http.StatusUnauthorized: true, http.StatusBadRequest: false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		pm := &Premiumize{Host: server.URL, client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))}
		pm.accountsManager = account.NewManager(
			config.Debrid{Name: "premiumize", DownloadAPIKeys: []string{"token"}},
			types.ProviderOptions{Logger: zerolog.Nop()},
			nil,
		)
		_, err := pm.GetDownloadLink(t.Context(), "id", &types.File{ID: "file-id"})
		server.Close()
		var typed *customerror.Error
		if !errors.As(err, &typed) || typed.IsPermanent() != permanent {
			t.Errorf("status %d: error = %v, want a typed error with permanent = %v", status, err, permanent)
		}
	}
}
