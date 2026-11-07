package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// A parse cut short by shutdown that surfaces as a missing article says nothing
// about the release, so it must not be shared with the network as "incomplete".
func TestParseStageReportsMissingSegmentsToHearsayOnlyWhileRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		cancelled bool
		reported  bool
	}{
		{name: "running", cancelled: false, reported: true},
		{name: "shutting down", cancelled: true, reported: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := map[string]*parser.FileGroup{"a": {
				BaseName: "a",
				Files:    []manifest.File{{Segments: []manifest.Segment{{Number: 1, MessageID: "a@example"}}}},
			}}
			m := withTestConfig(t, newParseRetryManager(
				t,
				func(context.Context, string, string, []byte, string) (*storage.NZB, map[string]*parser.FileGroup, error) {
					return nil, groups, customerror.ErrUsenetSegmentMissing
				},
			))
			cfg := &config.Config{}
			cfg.WithDir(t.TempDir())
			cfg.Hearsay.AdviceMode = "active"
			cfg.Usenet.Providers = []config.UsenetProvider{{Host: "news.example", Backbone: "omicron"}}
			service, err := hearsay.New(cfg, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			m.hearsay = service
			staged := filepath.Join(t.TempDir(), "Show.S01.nzb")
			if err = os.WriteFile(staged, []byte("<nzb/>"), 0o600); err != nil {
				t.Fatal(err)
			}
			entry := storedEntry(t, m)
			entry.Magnet = staged
			job := NewJob(JobTypeNZB, &ImportRequest{ID: "nzb-1", Name: "Show.S01", Arr: m.arr.GetOrCreate("sonarr")})
			job.Entry = entry

			ctx, cancel := context.WithCancel(t.Context())
			if tc.cancelled {
				cancel()
			}
			defer cancel()
			if parseErr := m.parseQueuedNZB(ctx, job); parseErr == nil {
				t.Fatal("parseQueuedNZB() = nil, want the parse failure")
			}

			if got := service.NZBClaimedIncomplete(hearsay.NZBSubjectFromGroups(groups)); got != tc.reported {
				t.Fatalf("hearsay claims the release incomplete = %v, want %v", got, tc.reported)
			}
		})
	}
}
