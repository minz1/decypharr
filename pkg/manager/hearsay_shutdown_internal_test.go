package manager

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
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
			m, call := newParseChildCallWith(
				t,
				func(context.Context, string, string, []byte, string) (*parser.Parsed, error) {
					return &parser.Parsed{
						Groups:  groups,
						Outcome: parser.Outcome{Verdict: parser.VerdictArticlesMissing},
					}, customerror.ErrUsenetSegmentMissing
				},
				oneFile(48*time.Hour),
			)
			m.pruneNow = causeNow
			cfg := &config.Config{}
			cfg.WithDir(t.TempDir())
			cfg.Hearsay.AdviceMode = "active"
			cfg.Usenet.Providers = []config.UsenetProvider{{Host: "news.example", Backbone: "omicron"}}
			service, err := hearsay.New(testGroup(t), cfg, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			m.hearsay = service

			ctx, cancel := context.WithCancel(t.Context())
			if tc.cancelled {
				cancel()
			}
			defer cancel()
			if msg := m.parseChild(ctx, call); msg.ev != nil == tc.cancelled {
				t.Fatalf("parse result = %+v, want a failure only while running", msg.ev)
			}

			if got := service.NZBClaimedIncomplete(hearsay.NZBSubjectFromGroups(groups)); got != tc.reported {
				t.Fatalf("hearsay claims the release incomplete = %v, want %v", got, tc.reported)
			}
		})
	}
}

func newHearsayService(t *testing.T) *hearsay.Service {
	t.Helper()
	cfg := &config.Config{}
	cfg.WithDir(t.TempDir())
	cfg.Hearsay.AdviceMode = "active"
	cfg.Usenet.Providers = []config.UsenetProvider{{Host: "news.example", Backbone: "omicron"}}
	service, err := hearsay.New(testGroup(t), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

func TestProcessBranchHearsayFollowsGateFact(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		result   usenet.ProcessResult
		err      error
		reported bool
	}{
		{"gate miss", usenet.ProcessResult{GateMissing: true}, customerror.ErrUsenetSegmentMissing, true},
		{
			"articles missing verdict",
			usenet.ProcessResult{Outcome: parser.Outcome{Verdict: parser.VerdictArticlesMissing}},
			customerror.ErrUsenetSegmentMissing, false,
		},
		{
			"segments missing verdict",
			usenet.ProcessResult{Outcome: parser.Outcome{Verdict: parser.VerdictSegmentsMissing}},
			holes(), false,
		},
		{"corrupt content", usenet.ProcessResult{}, customerror.ErrUsenetCorruptContent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := parsedGroups()
			m, call := newParseChildCallWith(
				t,
				func(context.Context, string, string, []byte, string) (*parser.Parsed, error) {
					return &parser.Parsed{NZB: &storage.NZB{ID: "nzb-1", Name: "Show.S01"}, Groups: groups}, nil
				},
				oneFile(48*time.Hour),
			)
			m.pruneNow = causeNow
			m.usenetTimeout = time.Minute
			m.processNZBFunc = func(context.Context, *parser.Parsed) (*storage.NZB, usenet.ProcessResult, error) {
				return nil, tc.result, tc.err
			}
			service := newHearsayService(t)
			m.hearsay = service

			if msg := m.parseChild(t.Context(), call); msg.ev == nil {
				t.Fatal("process failure produced no failure event")
			}
			if got := service.NZBClaimedIncomplete(hearsay.NZBSubjectFromGroups(groups)); got != tc.reported {
				t.Fatalf("hearsay claims the release incomplete = %v, want %v", got, tc.reported)
			}
		})
	}
}

func TestParseBranchHearsayFollowsCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		age      time.Duration
		verdict  parser.Verdict
		err      error
		groups   map[string]*parser.FileGroup
		reported bool
	}{
		{"old with verdict", 48 * time.Hour, parser.VerdictArticlesMissing, customerror.ErrUsenetSegmentMissing, parsedGroups(), true},
		{"fresh with verdict", time.Hour, parser.VerdictArticlesMissing, customerror.ErrUsenetSegmentMissing, parsedGroups(), false},
		{"sentinel without verdict", 48 * time.Hour, parser.VerdictNone, customerror.ErrUsenetSegmentMissing, parsedGroups(), false},
		{"empty subject", 48 * time.Hour, parser.VerdictArticlesMissing, customerror.ErrUsenetSegmentMissing, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, call := newParseChildCallWith(
				t,
				func(context.Context, string, string, []byte, string) (*parser.Parsed, error) {
					return &parser.Parsed{Groups: tc.groups, Outcome: parser.Outcome{Verdict: tc.verdict}}, tc.err
				},
				oneFile(tc.age),
			)
			m.pruneNow = causeNow
			service := newHearsayService(t)
			m.hearsay = service

			if msg := m.parseChild(t.Context(), call); msg.ev == nil {
				t.Fatal("parse failure produced no failure event")
			}
			if got := service.NZBClaimedIncomplete(hearsay.NZBSubjectFromGroups(parsedGroups())); got != tc.reported {
				t.Fatalf("hearsay claims the release incomplete = %v, want %v", got, tc.reported)
			}
		})
	}
}
