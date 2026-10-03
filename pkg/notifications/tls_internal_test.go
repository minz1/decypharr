package notifications

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// Notification callbacks trust the configured CA, so a callback endpoint
// behind a private certificate receives them.
func TestCallbackTrustsConfiguredCA(t *testing.T) {
	t.Parallel()
	var received atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())

	cfg := &config.Notifications{Enabled: true, CallbackURL: server.URL}
	service := New(cfg, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, zerolog.Nop())
	for _, notifier := range service.notifiers {
		if err := notifier.Send(t.Context(), Event{Type: config.EventDownloadComplete}); err != nil {
			t.Fatalf("%s: %v", notifier.Name(), err)
		}
	}
	if received.Load() != 1 {
		t.Fatalf("callback received %d notifications, want 1", received.Load())
	}
}
