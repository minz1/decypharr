package manager

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// One provider blocking a release while another failed transiently is no permanent refusal of the release.
func TestJoinedAddErrorIsPermanentOnlyWhenEveryProviderRefusedForGood(t *testing.T) {
	t.Parallel()
	transient := errors.New("torbox: giving up after 4 attempt(s)")
	for name, tc := range map[string]struct {
		errs          map[string]error
		wantCode      string
		wantPermanent bool
		wantThrottled []string
	}{
		"blocked and transient": {
			errs:     map[string]error{"realdebrid": customerror.ErrTorrentBlocked, "torbox": transient},
			wantCode: "provider_error",
		},
		"blocked and not cached": {
			errs:     map[string]error{"realdebrid": customerror.ErrTorrentBlocked, "torbox": customerror.ErrTorrentNotCached},
			wantCode: "provider_error",
		},
		"blocked and too many active downloads": {
			errs: map[string]error{
				"realdebrid": customerror.ErrTorrentBlocked, "torbox": customerror.ErrTooManyActiveDownloads,
			},
			wantCode:      "provider_error",
			wantThrottled: []string{"torbox"},
		},
		"blocked everywhere": {
			errs: map[string]error{
				"realdebrid": customerror.ErrTorrentBlocked, "torbox": customerror.ErrTorrentBlocked,
			},
			wantCode:      "torrent_blocked",
			wantPermanent: true,
		},
		"blocked on the only provider": {
			errs:          map[string]error{"realdebrid": customerror.ErrTorrentBlocked},
			wantCode:      "torrent_blocked",
			wantPermanent: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
			for provider, err := range tc.errs {
				m.clients.Store(provider, &throttlingProvider{name: provider, err: err})
			}
			_, err := m.SendToDebrid(&ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}})
			typed, ok := errors.AsType[*customerror.Error](err)
			if !ok {
				t.Fatalf("err = %v, want a classified error", err)
			}
			if typed.Code != tc.wantCode || typed.IsPermanent() != tc.wantPermanent ||
				typed.IsRetryable() == tc.wantPermanent {
				t.Fatalf("classified %q permanent=%t retryable=%t, want %q permanent=%t",
					typed.Code, typed.IsPermanent(), typed.IsRetryable(), tc.wantCode, tc.wantPermanent)
			}
			throttled, _ := errors.AsType[*throttledError](err)
			if tc.wantThrottled != nil && (!isTooManyActiveDownloads(err) || throttled == nil ||
				!slices.Equal(throttled.providers, tc.wantThrottled)) {
				t.Fatalf("throttled = %v (too many active %t), want providers %v",
					throttled, isTooManyActiveDownloads(err), tc.wantThrottled)
			}
		})
	}
}

// addProvider scripts a provider's submit and status check for the add path.
type addProvider struct {
	debrid.Client

	submit  func(ctx context.Context, t *types.Torrent) (*types.Torrent, error)
	check   func(ctx context.Context, t *types.Torrent) (*types.Torrent, error)
	deleted atomic.Int64
}

func (*addProvider) Config() config.Debrid {
	return config.Debrid{Name: "provider", Provider: "provider"}
}
func (*addProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *addProvider) SubmitMagnet(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	return p.submit(ctx, t)
}

func (p *addProvider) CheckStatus(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	return p.check(ctx, t)
}

func (p *addProvider) DeleteTorrent(context.Context, string) error {
	p.deleted.Add(1)
	return nil
}

// submittedTorrent is what a provider answers a submit with.
func submittedTorrent(t *types.Torrent) (*types.Torrent, error) {
	return &types.Torrent{
		ID: "pid", Debrid: "provider", InfoHash: t.InfoHash, Name: t.Name,
		Status: types.TorrentStatusDownloading, Files: map[string]types.File{},
	}, nil
}

// noTorrent is a status check's answer with neither a torrent nor an error, which Torbox and AllDebrid can give.
func noTorrent() (*types.Torrent, error) {
	var none *types.Torrent
	return none, nil
}

// newAddTestManager builds a manager whose only provider is provider.
func newAddTestManager(t *testing.T, provider debrid.Client) *Manager {
	t.Helper()
	m, _ := newDroppedTestManager(t, nil)
	m = withTestConfig(t, m)
	withLocalArr(m)
	m.clients.Store("provider", provider)
	return m
}

// withLocalArr points the manager's "sonarr" at a closed local port. An Arr refresh then fails at once, with no name
// lookup, which a synctest bubble cannot run.
func withLocalArr(m *Manager) {
	cfg := config.NewStore(&config.Config{Arrs: []config.Arr{{Name: "sonarr", Host: "http://127.0.0.1:1", Token: "x"}}})
	m.arr = arr.New(cfg, nil, zerolog.Nop())
}

func addRequest(t *testing.T) *ImportRequest {
	t.Helper()
	return &ImportRequest{
		Magnet:         &utils.Magnet{InfoHash: "hash", Name: "name"},
		Arr:            arr.Arr{Name: "sonarr"},
		DownloadFolder: t.TempDir(),
	}
}

func submitCall(t *testing.T) entryChildCall {
	t.Helper()
	return entryChildCall{
		kind: grab.ChildSubmit,
		grab: grab.Grab{Entry: &storage.Entry{InfoHash: "hash", Name: "name"}},
		req:  addRequest(t),
	}
}

// A stop during SubmitMagnet (no ID yet) reports no result and deletes nothing: the grab looks the submit up.
func TestSubmitChildStopDuringSubmitReportsNothing(t *testing.T) {
	t.Parallel()
	submitting := make(chan struct{})
	provider := &addProvider{submit: func(ctx context.Context, _ *types.Torrent) (*types.Torrent, error) {
		close(submitting)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	m := newAddTestManager(t, provider)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan entryMsg, 1)
	go func() { result <- m.submitChild(ctx, submitCall(t)) }()
	<-submitting
	cancel()
	if msg := <-result; msg.ev != nil {
		t.Fatalf("a submit cut by a stop reported %+v, want no result", msg.ev)
	}
	closePlacements(t, m)
	if got := provider.deleted.Load(); got != 0 {
		t.Fatalf("provider deletes = %d, want none", got)
	}
}

// A stop after the provider took the magnet is no verdict on it: whatever the status check made of the stop, the
// submit reports the placement and the torrent stays at the provider.
func TestSubmitChildStopDuringPostSubmitCheckKeepsTorrent(t *testing.T) {
	t.Parallel()
	for name, result := range map[string]func(*types.Torrent) (*types.Torrent, error){
		"http cancel":   func(torrent *types.Torrent) (*types.Torrent, error) { return torrent, context.Canceled },
		"nil torrent":   func(*types.Torrent) (*types.Torrent, error) { return noTorrent() },
		"input torrent": func(torrent *types.Torrent) (*types.Torrent, error) { return torrent, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			provider := &addProvider{
				submit: func(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
					return submittedTorrent(torrent)
				},
			}
			provider.check = func(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
				cancel()
				return result(torrent)
			}
			m := newAddTestManager(t, provider)
			msg := m.submitChild(ctx, submitCall(t))
			if msg.ev == nil || msg.ev.Outcome != grab.OutcomePlacement || msg.torrent == nil ||
				msg.torrent.ID != "pid" {
				t.Fatalf("submit result = %+v, want the placement of pid", msg)
			}
			closePlacements(t, m)
			if got := provider.deleted.Load(); got != 0 {
				t.Fatalf("provider deletes = %d, want none", got)
			}
		})
	}
}

// A SubmitMagnet result need not name its provider (AllDebrid sets only the ID), so a stop after the submit completes
// the result from the request: provider name, hash and the DownloadUncached choice.
func TestSubmitChildStopAfterSubmitSetsProvider(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	provider := &addProvider{}
	provider.submit = func(context.Context, *types.Torrent) (*types.Torrent, error) {
		cancel()
		return &types.Torrent{ID: "pid"}, nil
	}
	provider.check = func(context.Context, *types.Torrent) (*types.Torrent, error) { return noTorrent() }
	m := newAddTestManager(t, provider)
	uncached := true
	call := submitCall(t)
	call.req.DownloadUncached = &uncached

	msg := m.submitChild(ctx, call)
	got := msg.torrent
	if msg.ev == nil || got == nil {
		t.Fatalf("submit result = %+v, want the submitted torrent", msg)
	}
	if got.ID != "pid" || got.Debrid != "provider" || got.InfoHash != "hash" || !got.DownloadUncached ||
		got.Status != types.TorrentStatusDownloading {
		t.Fatalf("torrent = %+v, want pid on provider, hash, uncached, downloading", got)
	}
	closePlacements(t, m)
	if deleted := provider.deleted.Load(); deleted != 0 {
		t.Fatalf("provider deletes = %d, want none", deleted)
	}
}
