package request

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

type failingCloseBody struct{ io.Reader }

func (failingCloseBody) Close() error { return errors.New("connection reset") }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A client logs its diagnostics through the logger it was built with; they
// used to go to a no-op logger unless a caller remembered an option.
func TestClientLogsThroughItsLogger(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	client := New(zerolog.New(&logs), nil, WithMaxRetries(0))
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       failingCloseBody{strings.NewReader("ok")},
			Request:    r,
		}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://provider.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.MakeRequest(req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "connection reset") {
		t.Fatalf("logs = %q, want the close failure", logs.String())
	}
}
