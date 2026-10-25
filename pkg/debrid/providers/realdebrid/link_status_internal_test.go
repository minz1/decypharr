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
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// A link RealDebrid refuses for good stops a read at once; a status that may pass does not.
func TestDownloadLinkErrorsAreTyped(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status    int
		body      string
		permanent bool
	}{
		"unknown resource": {http.StatusNotFound, `{"error":"unknown_ressource","error_code":7}`, true},
		"bad token":        {http.StatusUnauthorized, `{"error":"bad_token","error_code":8}`, true},
		"bad request":      {http.StatusBadRequest, `{"error":"parameter_missing","error_code":2}`, false},
		"no link":          {http.StatusOK, `{"download":""}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			provider := &RealDebrid{Host: server.URL}
			provider.accountsManager = account.NewManager(
				config.Debrid{Name: "realdebrid", DownloadAPIKeys: []string{"token"}},
				types.ProviderOptions{Logger: zerolog.Nop()},
				nil,
			)
			_, err := provider.GetDownloadLink(t.Context(), "id", &types.File{Link: "https://example.test/file"})
			var typed *customerror.Error
			if !errors.As(err, &typed) || typed.IsPermanent() != tc.permanent {
				t.Fatalf("GetDownloadLink error = %v, want a typed error with permanent = %v", err, tc.permanent)
			}
		})
	}
}
