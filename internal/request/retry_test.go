package request_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/request"
)

const activeDownloadsLimit = `{"error":"active_downloads_limit"}`

func TestRetryPolicyPreservesUnlistedProviderStatus(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider response", true: "configured retry"}[explicit], func(t *testing.T) {
			t.Parallel()
			checkRetryPolicy(t, explicit)
		})
	}
}

func checkRetryPolicy(t *testing.T, explicit bool) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(509)
		_, _ = io.WriteString(w, activeDownloadsLimit)
	}))
	defer server.Close()
	opts := []request.ClientOption{request.WithMaxRetries(0)}
	if explicit {
		opts = append(opts, request.WithRetryableStatus(509))
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := request.New(zerolog.Nop(), nil, opts...).Do(req)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if explicit {
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("configured retry did not reach its limit")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 509 || string(body) != activeDownloadsLimit {
		t.Fatalf("response = %d %q, error = %v", resp.StatusCode, body, err)
	}
}
