package request_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/request"
)

func TestParseProxy(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"http://proxy:3128":        "http://proxy:3128",
		"socks5://u:p@proxy:1080":  "socks5://u:p@proxy:1080",
		"socks5h://proxy:1080":     "socks5h://proxy:1080",
		"proxy.lan:3128":           "http://proxy.lan:3128",
		"\thttps://proxy:443\n":    "https://proxy:443",
		"http://[::1":              "",
		"ftp://proxy:21":           "",
		"http://":                  "",
		"socks4://proxy:1080":      "",
		"http://proxy:3128/%zzbad": "",
	} {
		got, err := request.ParseProxy(raw)
		switch {
		case want == "" && err == nil:
			t.Errorf("ParseProxy(%q) = %v, want an error", raw, got)
		case want != "" && (err != nil || got.String() != want):
			t.Errorf("ParseProxy(%q) = %v, %v; want %s", raw, got, err, want)
		}
	}
}

// A proxy URL that cannot be used fails requests instead of letting them go
// out directly, bypassing the proxy the user configured.
func TestInvalidProxyFailsClosed(t *testing.T) {
	t.Parallel()
	var direct atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := request.New(request.WithProxy("http://[::1"), request.WithMaxRetries(0))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request through an invalid proxy succeeded")
	}
	if direct.Load() != 0 {
		t.Fatal("request bypassed the configured proxy")
	}
}

// A socks5h:// proxy (host names resolved by the proxy) is dialed through,
// not refused as invalid.
func TestSetProxyAcceptsSocks5h(t *testing.T) {
	t.Parallel()
	transport := &http.Transport{}
	request.SetProxy(transport, "socks5h://user:pass@127.0.0.1:1080")
	if transport.Proxy != nil || transport.DialContext == nil {
		t.Fatal("socks5h proxy was not configured as a SOCKS5 dialer")
	}
}
