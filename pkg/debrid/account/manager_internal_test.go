package account

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"go.uber.org/ratelimit"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func newTestManager(logOutput *bytes.Buffer) (*Manager, *Account) {
	acc := &Account{
		Debrid: "realdebrid",
		Token:  "token",
		links:  xsync.NewMap[string, types.DownloadLink](),
	}
	accounts := xsync.NewMap[string, *Account]()
	accounts.Store(acc.Token, acc)
	m := &Manager{
		debrid:   acc.Debrid,
		accounts: accounts,
		logger:   zerolog.New(logOutput),
	}
	m.current.Store(acc)
	return m, acc
}

func TestSyncReenablesHealthyDisabledAccount(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m, acc := newTestManager(&logs)
	m.Disable(acc)

	m.Sync(func(*Account) error { return nil })

	if acc.Disabled.Load() {
		t.Fatal("expected a successful sync to re-enable the account")
	}
	if got := m.Current(); got != acc {
		t.Fatalf("expected the recovered account to be current, got %#v", got)
	}
}

func TestSyncKeepsAccountDisabledOnFailure(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m, acc := newTestManager(&logs)
	m.Disable(acc)

	m.Sync(func(*Account) error { return errors.New("temporary failure") })

	if !acc.Disabled.Load() {
		t.Fatal("expected a failed sync to leave the account disabled")
	}
}

func TestNoActiveAccountWarningIsThrottled(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m, acc := newTestManager(&logs)
	m.Disable(acc)

	for range 10 {
		_ = m.Current()
	}

	if got := strings.Count(logs.String(), "No active accounts"); got != 1 {
		t.Fatalf("expected one no-active-account warning, got %d: %s", got, logs.String())
	}
}

func TestInvalidFetchedLinkIsNotCached(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	_, acc := newTestManager(&logs)
	file := &types.File{Link: "https://example.test/file"}
	calls := 0
	fetcher := func(context.Context, *Account, string, *types.File) (types.DownloadLink, error) {
		calls++
		dl := types.DownloadLink{Link: file.Link, ExpiresAt: time.Now().Add(time.Hour)}
		if calls > 1 {
			dl.DownloadLink = "https://cdn.example.test/file"
		}
		return dl, nil
	}
	if _, err := acc.GetDownloadLink(t.Context(), "id", file, fetcher); !errors.Is(err, types.ErrEmptyDownloadLink) {
		t.Fatalf("first fetch error = %v, want empty link", err)
	}
	dl, err := acc.GetDownloadLink(t.Context(), "id", file, fetcher)
	if err != nil || calls != 2 || dl.DownloadLink == "" {
		t.Fatalf("second fetch: link=%q calls=%d err=%v, want refetched link", dl.DownloadLink, calls, err)
	}
}

func TestMeasureDownloadIsBoundedWhenRangeIgnored(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4*speedTestBytes)) // ignores Range, sends 200 with the whole body
	}))
	defer server.Close()
	var logs bytes.Buffer
	m, acc := newTestManager(&logs)
	acc.httpClient = request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))
	acc.storeLink(types.DownloadLink{Link: "file", DownloadLink: server.URL})
	var result types.SpeedTestResult
	m.MeasureDownload(t.Context(), &result)
	if result.BytesRead != speedTestBytes {
		t.Fatalf("BytesRead = %d, want %d", result.BytesRead, speedTestBytes)
	}
}

func TestDownloadLinkPropagatesFailuresAndCancellation(t *testing.T) {
	t.Parallel()
	firstErr, secondErr := errors.New("first failed"), errors.New("second failed")
	for _, tc := range []struct {
		name         string
		cancelBefore bool
		cancelDuring bool
		wantCalls    int
		wantErrs     []error
	}{
		{name: "all fail", wantCalls: 2, wantErrs: []error{firstErr, secondErr}},
		{name: "cancel before", cancelBefore: true, wantCalls: 0, wantErrs: []error{context.Canceled}},
		{name: "cancel during", cancelDuring: true, wantCalls: 1, wantErrs: []error{context.Canceled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			m, first := newTestManager(&logs)
			second := &Account{Token: "second", Debrid: first.Debrid, links: xsync.NewMap[string, types.DownloadLink]()}
			m.accounts.Store(second.Token, second)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			calls := 0
			fetch := func(got context.Context, acc *Account, _ string, _ *types.File) (types.DownloadLink, error) {
				calls++
				if got != ctx {
					t.Error("caller context was replaced")
				}
				switch {
				case tc.cancelDuring:
					cancel()
					return types.DownloadLink{}, ctx.Err()
				case acc == first:
					return types.DownloadLink{}, firstErr
				default:
					return types.DownloadLink{}, secondErr
				}
			}
			_, err := m.GetDownloadLink(ctx, "id", &types.File{Link: "file"}, fetch)
			if calls != tc.wantCalls {
				t.Fatalf("fetch calls=%d, want %d", calls, tc.wantCalls)
			}
			if !isAll(err, tc.wantErrs) {
				t.Fatalf("error=%v, want all of %v", err, tc.wantErrs)
			}
		})
	}
}

// isAll reports whether err matches every target.
func isAll(err error, targets []error) bool {
	for _, target := range targets {
		if !errors.Is(err, target) {
			return false
		}
	}
	return true
}

// Sync writes an account's profile while stats read it (run with -race).
func TestAccountProfileIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	acc := &Account{}
	expiry := time.Now().Add(time.Hour)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			acc.SetProfile("user", expiry)
		}
	})
	wg.Go(func() {
		for range 200 {
			_ = acc.Username()
			_ = acc.Expiration()
		}
	})
	wg.Wait()
	if acc.Username() != "user" || !acc.Expiration().Equal(expiry) {
		t.Fatalf("profile = %q, %v", acc.Username(), acc.Expiration())
	}
}

type countingLimiter struct{ takes atomic.Int64 }

func (l *countingLimiter) Take() time.Time {
	l.takes.Add(1)
	return time.Now()
}

// Each download account spends the bucket of the key it authenticates with.
func TestNewManagerWithLimitersPicksLimiterPerKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	main, download := &countingLimiter{}, &countingLimiter{}
	dc := config.Debrid{Name: "torbox", APIKey: "key-1", DownloadAPIKeys: []string{"key-1", "key-2"}}
	m := NewManagerWithLimiters(dc, types.ProviderOptions{Logger: zerolog.Nop()}, func(token string) ratelimit.Limiter {
		if token == dc.APIKey {
			return main
		}
		return download
	})
	for _, token := range dc.DownloadAPIKeys {
		acc, ok := m.accounts.Load(token)
		if !ok {
			t.Fatalf("no account for %s", token)
		}
		resp, err := acc.httpClient.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if main.takes.Load() != 1 || download.takes.Load() != 1 {
		t.Fatalf("takes main=%d download=%d, want 1 and 1", main.takes.Load(), download.takes.Load())
	}
}
