package repair

import (
	"fmt"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A hash the provider dropped still passes the stored-link check; only a fresh unrestrict reads it gone, and with
// Fix the entry is re-inserted instead of reacquired.
func TestRecheckEntryUnrestrictLinkFindsADroppedTorrent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		unrestrict bool
		fix        bool
		want       storage.HealthStatus
		reason     string
		reinserts  int
	}{
		{name: "stored link", want: storage.HealthHealthy},
		{name: "unrestrict", unrestrict: true, want: storage.HealthBroken, reason: "torrent_not_found"},
		{name: "unrestrict and fix", unrestrict: true, fix: true, want: storage.HealthHealthy, reinserts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSweepFixture(t, "video.mkv")
			f.serveRadarr(t)
			placement := f.entry.Providers["removed"]
			placement.Files = map[string]*storage.ProviderFile{"video.mkv": {Link: "https://rd/video.mkv"}}
			if err := f.store.AddOrUpdate(f.entry); err != nil {
				t.Fatal(err)
			}
			f.backend.client = newLinkClient(map[string][]linkResult{
				"video.mkv": {{err: fmt.Errorf("gone: %w", customerror.ErrTorrentNotFound)}},
			})
			reinserts := 0
			f.backend.reinsert = func(*storage.Entry) error {
				reinserts++
				return nil
			}
			opts := RecheckOptions{Fix: tc.fix, UnrestrictLink: tc.unrestrict}
			if _, err := f.service.RecheckEntry(t.Context(), f.entry.Name, opts); err != nil {
				t.Fatal(err)
			}
			f.exits.wait()
			saved, _ := f.store.GetEntryHealth(f.entry.Name)
			if saved == nil || saved.Status != tc.want || saved.FailureReason != tc.reason {
				t.Fatalf("saved health = %#v, want %s with reason %q", saved, tc.want, tc.reason)
			}
			if reinserts != tc.reinserts || len(f.reacquirer.requests) != 0 {
				t.Fatalf("reinserts = %d, reacquisitions = %#v; want %d and none",
					reinserts, f.reacquirer.requests, tc.reinserts)
			}
		})
	}
}
