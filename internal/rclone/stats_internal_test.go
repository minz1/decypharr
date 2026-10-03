package rclone

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/request"
)

func TestStatsPreservesCountersAndPartialResults(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/core/stats":
			_, _ = fmt.Fprint(w, `{"bytes":9007199254740993}`)
		case "/core/memstats":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case "/core/bwlimit":
			_, _ = fmt.Fprint(w, `{"bytesPerSecond":123,"rate":"123B"}`)
		case "/core/version":
			_, _ = fmt.Fprint(w, `{"version":"test"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{baseURL: server.URL, client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))}
	stats := client.Stats(t.Context())
	core, ok := stats["core"].(CoreStatsResponse)
	if !ok || core.Bytes != 9007199254740993 {
		t.Fatalf("core stats lost type or precision: %#v", stats["core"])
	}
	if memory, memOK := stats["memory"].(MemoryStats); !memOK || memory != (MemoryStats{}) {
		t.Fatalf("failed memory section = %#v", stats["memory"])
	}
	if stats["bandwidth"].(BandwidthStats).BytesPerSecond != 123 ||
		stats["version"].(VersionResponse).Version != "test" {
		t.Fatal("successful sections were lost")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, endpoint := range []string{"/core/stats", "/core/memstats", "/core/bwlimit", "/core/version"} {
		if calls[endpoint] != 1 {
			t.Errorf("%s called %d times", endpoint, calls[endpoint])
		}
	}
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Core CoreStatsResponse `json:"core"`
	}
	if unmarshalErr := json.Unmarshal(data, &payload); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if payload.Core.Bytes != core.Bytes {
		t.Fatal("serialized counter lost precision")
	}
}

// The RC client trusts the configured CA (tls_ca_file), so an rclone RC
// endpoint behind a private certificate works.
func TestClientUsesConfiguredTLS(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"version":"test"}`)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())

	client := NewClient(server.URL, "", "", &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, zerolog.Nop())
	var version VersionResponse
	if err := client.Do(t.Context(), Request{Command: "core/version"}, &version); err != nil {
		t.Fatalf("RC call over the configured CA: %v", err)
	}
	if version.Version != "test" {
		t.Fatalf("version = %+v", version)
	}
}
