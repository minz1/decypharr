package qbit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestHandleLoginAlwaysReturnsSID(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/auth/login",
		strings.NewReader("username=homarr-user&password=homarr-password"),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	(&QBit{manager: mgr, config: store}).handleLogin(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d", recorder.Code, http.StatusOK)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "SID" || cookies[0].Value == "" {
		t.Fatalf("login cookies = %#v, want one SID cookie", cookies)
	}
	if !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("SID cookie = %#v, want Secure and HttpOnly", cookies[0])
	}
	username, password, err := extractFromSID(store.Get().SecretKey(), cookies[0].Value)
	if err != nil {
		t.Fatalf("decode SID: %v", err)
	}
	if username != "homarr-user" || password != "homarr-password" {
		t.Fatalf("SID credentials = %q/%q", username, password)
	}
}

func TestWriteTorrentAddErrorPreservesSemantics(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		err       error
		status    int
		code      string
		retryable bool
		permanent bool
	}{
		"blocked": {
			err:       fmt.Errorf("submit: %w", customerror.ErrTorrentBlocked),
			status:    http.StatusUnavailableForLegalReasons,
			code:      "torrent_blocked",
			permanent: true,
		},
		"not cached": {
			err:       fmt.Errorf("status: %w", customerror.ErrTorrentNotCached),
			status:    http.StatusNotFound,
			code:      "torrent_not_cached",
			retryable: true,
		},
		"invalid input": {
			err: customerror.NewError(errors.New("bad magnet"), http.StatusBadRequest, "invalid_magnet", false, false).
				Permanent(),
			status:    http.StatusBadRequest,
			code:      "invalid_magnet",
			permanent: true,
		},
		"blocked on one provider, transient on another": {
			err: customerror.NewError(fmt.Errorf("failed to process torrent: %w", errors.Join(
				fmt.Errorf("realdebrid: %w", customerror.ErrTorrentBlocked),
				errors.New("torbox: giving up after 4 attempt(s)"),
			)), http.StatusBadGateway, "provider_error", false, false).Retryable(),
			status:    http.StatusBadGateway,
			code:      "provider_error",
			retryable: true,
		},
		"provider failure": {
			err:       errors.New("connection reset"),
			status:    http.StatusBadGateway,
			code:      "provider_error",
			retryable: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			writeTorrentAddError(recorder, tt.err)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q, want application/json", got)
			}

			var response torrentAddErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Code != tt.code || response.Retryable != tt.retryable || response.Permanent != tt.permanent {
				t.Fatalf(
					"response = %#v, want code=%q retryable=%t permanent=%t",
					response,
					tt.code,
					tt.retryable,
					tt.permanent,
				)
			}
		})
	}
}

type refusingProvider struct {
	debrid.Client

	name    string
	err     error
	submits atomic.Int64
}

func (p *refusingProvider) Config() config.Debrid {
	return config.Debrid{Name: p.name, Provider: p.name}
}
func (p *refusingProvider) Logger() zerolog.Logger     { return zerolog.Nop() }
func (p *refusingProvider) DeleteTorrent(string) error { return nil }
func (p *refusingProvider) SubmitMagnet(*types.Torrent) (*types.Torrent, error) {
	p.submits.Add(1)
	return nil, p.err
}

// An add RealDebrid blocks while TorBox is at its active-download cap is accepted as queued and retried when a
// TorBox slot frees, not when a RealDebrid one does.
func TestAddBlockedOnOneProviderThrottledOnAnotherIsQueued(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) { cfg.UseAuth = false })
	realDebrid := &refusingProvider{name: "realdebrid", err: customerror.ErrTorrentBlocked}
	torBox := &refusingProvider{
		name: "torbox", err: fmt.Errorf("torbox queued it: %w", customerror.ErrTooManyActiveDownloads),
	}
	mgr.Clients().Store(realDebrid.name, realDebrid)
	mgr.Clients().Store(torBox.name, torBox)
	routes := New(mgr, store, zerolog.Nop()).Routes()

	const hash = "0123456789abcdef0123456789abcdef01234567"
	form := url.Values{"urls": {"magnet:?xt=urn:btih:" + hash + "&dn=release"}, "category": {"sonarr"}}
	if w := postForm(routes, "/torrents/add", form); w.Code != http.StatusOK {
		t.Fatalf("add status = %d: %s", w.Code, w.Body.String())
	}
	if realDebrid.submits.Load() != 1 || torBox.submits.Load() != 1 {
		t.Fatalf("submits realdebrid=%d torbox=%d, want 1 each", realDebrid.submits.Load(), torBox.submits.Load())
	}

	w := httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/torrents/info", nil))
	var listed []Torrent
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if listed[0].Hash != hash || listed[0].State != "queuedDL" {
		t.Fatalf("listed %s in state %q, want %s queuedDL", listed[0].Hash, listed[0].State, hash)
	}

	slot := func(provider string) *storage.ProviderEntry {
		return &storage.ProviderEntry{Provider: provider, ID: "other", Status: types.TorrentStatusDownloading}
	}
	if err := mgr.RemoveFromProvider(slot(realDebrid.name)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := torBox.submits.Load(); got != 1 {
		t.Fatalf("a freed realdebrid slot resubmitted the add (torbox submits = %d)", got)
	}
	if err := mgr.RemoveFromProvider(slot(torBox.name)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); torBox.submits.Load() < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a freed torbox slot did not resubmit the add")
		}
	}
	entry, err := mgr.Queue().GetTorrent(hash)
	if err != nil || entry.Status != types.TorrentStatusQueued {
		t.Fatalf("entry after the retry = %+v, %v; want still queued", entry, err)
	}
}
