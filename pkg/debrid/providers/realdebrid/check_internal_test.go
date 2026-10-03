package realdebrid

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/common/commontest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestCheckFileHonorsCancellation(t *testing.T) {
	t.Parallel()
	newProvider := func(host string) *RealDebrid {
		provider := &RealDebrid{Host: host, repairClient: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))}
		provider.accountsManager = account.NewManager(
			config.Debrid{Name: "realdebrid", DownloadAPIKeys: []string{"token"}},
			types.ProviderOptions{Logger: zerolog.Nop()},
			nil,
		)
		return provider
	}
	operations := map[string]func(ctx context.Context, host string) error{
		"check": func(ctx context.Context, host string) error {
			return newProvider(host).CheckFile(ctx, "hash", "file-id")
		},
		"download link": func(ctx context.Context, host string) error {
			_, err := newProvider(host).GetDownloadLink(ctx, "id", &types.File{Link: "https://example.test/file"})
			return err
		},
	}
	for operation, op := range operations {
		for name, cancelBefore := range commontest.CancelCases() {
			t.Run(operation+"/"+name, func(t *testing.T) {
				t.Parallel()
				commontest.AssertCancellation(t, cancelBefore, op)
			})
		}
	}
}

// Only a 2xx answer means the file is available, and only a 404 that it is
// gone; any other status is a failed probe.
func TestCheckFileStatus(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{
		http.StatusOK:                  "available",
		http.StatusNotFound:            "unavailable",
		http.StatusUnauthorized:        "error",
		http.StatusForbidden:           "error",
		http.StatusTooManyRequests:     "error",
		http.StatusInternalServerError: "error",
		http.StatusServiceUnavailable:  "error",
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			provider := &RealDebrid{
				Host:         server.URL,
				repairClient: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
			}
			err := provider.CheckFile(t.Context(), "hash", "https://real-debrid.com/d/file")
			var got string
			switch {
			case err == nil:
				got = "available"
			case errors.Is(err, customerror.ErrHosterUnavailable):
				got = "unavailable"
			default:
				got = "error"
			}
			if got != want {
				t.Fatalf("status %d: CheckFile = %v (%s), want %s", status, err, got, want)
			}
		})
	}
}
