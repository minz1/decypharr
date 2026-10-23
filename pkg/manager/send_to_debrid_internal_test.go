package manager

import (
	"errors"
	"slices"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
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
			_, err := m.SendToDebrid(t.Context(), &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}})
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
