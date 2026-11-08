package notifications

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

func notifiersFor(url string) []Notifier {
	client := &http.Client{Timeout: 5 * time.Second}
	return []Notifier{NewDiscord(url, client), NewCallback(url, client)}
}

func TestNotifierStatusErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		code       int
		retryAfter string
		body       string
		wantCode   int
		wantRA     time.Duration
	}{
		{"429 with retry-after", http.StatusTooManyRequests, "7", "", http.StatusTooManyRequests, 7 * time.Second},
		{"503", http.StatusServiceUnavailable, "", "", http.StatusServiceUnavailable, 0},
		{"400 keeps the body", http.StatusBadRequest, "", "bad embed", http.StatusBadRequest, 0},
		{"204 delivered", http.StatusNoContent, "", "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)
			for _, n := range notifiersFor(server.URL) {
				err := n.Send(t.Context(), Event{Type: config.EventDownloadComplete, Status: "success"})
				var want *StatusError
				if tt.wantCode != 0 {
					want = &StatusError{Code: tt.wantCode, RetryAfter: tt.wantRA, Body: tt.body}
				}
				checkStatusError(t, n.Name(), err, want)
			}
		})
	}
}

// checkStatusError says whether err is the StatusError want names (by code,
// Retry-After and body), or nil when want is nil.
func checkStatusError(t *testing.T, notifier string, err error, want *StatusError) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("%s: Send() = %v, want nil", notifier, err)
		}
		return
	}
	se, ok := errors.AsType[*StatusError](err)
	if !ok {
		t.Fatalf("%s: Send() = %v, want *StatusError", notifier, err)
	}
	if se.Code != want.Code || se.RetryAfter != want.RetryAfter || se.Body != want.Body {
		t.Fatalf("%s: StatusError = %+v, want code %d retry-after %v body %q",
			notifier, se, want.Code, want.RetryAfter, want.Body)
	}
}

func TestNotifierBadURLIsPermanent(t *testing.T) {
	t.Parallel()
	for _, n := range notifiersFor("http://[::1") {
		err := n.Send(t.Context(), Event{Type: config.EventDownloadComplete})
		if _, ok := errors.AsType[*permanentError](err); !ok {
			t.Fatalf("%s: Send() = %v, want a permanent error", n.Name(), err)
		}
	}
}
