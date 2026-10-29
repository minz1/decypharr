package repair

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestRollupCountsUncheckedFilesAsUnknown(t *testing.T) {
	t.Parallel()
	ok := fileResult{name: "a.mkv", healthy: true}
	for _, tc := range []struct {
		name   string
		other  fileResult
		want   storage.HealthStatus
		reason string
	}{
		{name: "provider error", other: fileResult{name: "b.mkv", reason: "provider_probe_error"},
			want: storage.HealthUnknown, reason: "provider_probe_error"},
		{name: "cancelled", other: fileResult{name: "b.mkv", reason: "context_cancelled"},
			want: storage.HealthUnknown, reason: "context_cancelled"},
		{name: "broken", other: fileResult{name: "b.mkv", broken: true, reason: "hoster_unavailable"},
			want: storage.HealthBroken},
		{name: "healthy", other: fileResult{name: "b.mkv", healthy: true}, want: storage.HealthHealthy},
	} {
		results := []fileResult{ok, tc.other}
		if got := rollupStatus(results); got != tc.want {
			t.Errorf("%s: rollupStatus = %s, want %s", tc.name, got, tc.want)
		}
		if tc.reason != "" && firstFailureReason(results) != tc.reason {
			t.Errorf("%s: firstFailureReason = %q, want %q", tc.name, firstFailureReason(results), tc.reason)
		}
	}
}

// A provider missing from the manager never makes a sweep reacquire; the entry reads unknown and says why.
func TestSweepNeverReacquiresForAMissingProvider(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.backend.client = nil
	f.probe(t.Context(), t, true)
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("sweeps queued %#v, want no reacquire for a missing provider", f.reacquirer.requests)
	}
	saved, _ := f.store.GetEntryHealth(f.entry.Name)
	if saved == nil || saved.Status != storage.HealthUnknown || saved.FailureReason != "provider_client_not_found" {
		t.Fatalf("saved health = %#v, want unknown with the missing-provider reason", saved)
	}
}

// The owner's per-entry recheck still sees a missing provider as broken, so Fix can act on it.
func TestRecheckEntryTreatsAMissingProviderAsBroken(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.backend.client = nil
	if _, err := f.service.RecheckEntry(t.Context(), f.entry.Name, false); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	saved, _ := f.store.GetEntryHealth(f.entry.Name)
	if saved == nil || saved.Status != storage.HealthBroken || saved.FailureReason != "provider_client_not_found" {
		t.Fatalf("saved health = %#v, want broken with the missing-provider reason", saved)
	}
}

func TestClearDisprovedMarksLogsKeptMark(t *testing.T) {
	t.Parallel()
	failing := healthy(confirmHash, "b.mkv")
	failing.healthy, failing.broken, failing.reason = false, true, "hoster_unavailable"
	for _, tc := range []struct {
		name    string
		results []fileResult
		want    string
	}{
		{name: "failing file", results: []fileResult{healthy(confirmHash, "a.mkv"), failing}, want: "hoster_unavailable"},
		{name: "unprobed file", results: []fileResult{healthy(confirmHash, "a.mkv")}, want: "file_not_probed"},
	} {
		var logs bytes.Buffer
		store := newRepairTestStorage(t)
		if err := store.AddOrUpdate(markedEntry(confirmHash, "a.mkv", "b.mkv")); err != nil {
			t.Fatal(err)
		}
		service := New(Dependencies{Storage: store, Backend: &clientBackend{}, Logger: zerolog.New(&logs)})
		service.clearDisprovedMarks(t.Context(), tc.results)
		if !strings.Contains(logs.String(), "bad mark kept") || !strings.Contains(logs.String(), tc.want) {
			t.Errorf("%s: log = %q, want a kept-mark line naming %q", tc.name, logs.String(), tc.want)
		}
	}
}
