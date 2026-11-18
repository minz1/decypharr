package torbox

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestAProviderOutageOnAStatusPollIsTransient(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]bool{
		http.StatusServiceUnavailable:  true,
		http.StatusInternalServerError: true,
		http.StatusTooManyRequests:     true,
		525:                            true,
		http.StatusUnauthorized:        false,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		provider := testTorbox(server.URL)
		_, _, err := provider.Poll(t.Context(), &types.Torrent{ID: "17"}, true)
		server.Close()
		if err == nil || customerror.IsRetriableError(err) != want {
			t.Errorf("status %d: err %v, retriable %t, want %t", status, err, customerror.IsRetriableError(err), want)
		}
	}
}
