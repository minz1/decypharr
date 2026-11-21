package usenet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/internal/gate"
)

func stepReturning(ran *[]string, name string, r gate.Result) gateStep {
	return gateStep{name: name, run: func() gate.Result {
		*ran = append(*ran, name)
		return r
	}}
}

func storedStatus(t *testing.T, u *Usenet, nzb *storage.NZB) string {
	t.Helper()
	header, err := u.nzbStorage.GetNZBHeader(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	return header.Status
}

func TestGateStepsRunAvailabilityThenContent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var commands []string
	u, srv := gateHarness(t, func(command, _ string) string {
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		return ""
	})
	payload := []byte(strings.Repeat("not a media container ", 40))
	srv.AddArticle("<a@gate>", nntpd.Encode(payload, "movie.mkv", 1, int64(len(payload)), 0))
	nzb := gateNZB(t, u, storage.NZBFile{
		Name: "Movie.mkv", FileType: storage.NZBFileTypeMedia,
		Segments: []storage.NZBSegment{{Number: 1, MessageID: "<a@gate>", Bytes: int64(len(payload))}},
	})
	steps := u.gateSteps(t.Context(), nzb)
	var names []string
	for _, step := range steps {
		names = append(names, step.name)
	}
	if want := []string{"availability check", "content verification"}; !slices.Equal(names, want) {
		t.Fatalf("steps = %v, want %v", names, want)
	}
	if r := steps[0].run(); r.Kind() != gate.KindPass {
		t.Fatalf("availability step = %v, %v, want pass on present articles", r.Kind(), r.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(commands, "STAT") || slices.ContainsFunc(commands, func(c string) bool { return c != "STAT" }) {
		t.Fatalf("availability step sent %v, want STAT only", commands)
	}
}

func TestFinishFailsClosedOnNoResultAfterAPass(t *testing.T) {
	t.Parallel()
	u, _ := gateHarness(t, nil)
	nzb := gateNZB(t, u, gateFile(8))
	var ran []string
	got, err := u.finish(t.Context(), nzb, []gateStep{
		stepReturning(&ran, "one", gate.Pass()),
		stepReturning(&ran, "two", gate.Result{}),
	})
	if err == nil || err.Error() != "two returned no result" || got != (ProcessResult{}) {
		t.Fatalf("finish = %+v, %v, want two returned no result", got, err)
	}
	if !slices.Equal(ran, []string{"one", "two"}) {
		t.Fatalf("steps run = %v, want both", ran)
	}
	if status := storedStatus(t, u, nzb); status != NZBStatusFailed {
		t.Fatalf("stored status = %q, want failed", status)
	}
}

func TestFinishFailsClosedOnAStepThatReturnsNoResult(t *testing.T) {
	t.Parallel()
	u, _ := gateHarness(t, nil)
	nzb := gateNZB(t, u, gateFile(8))
	var ran []string
	got, err := u.finish(t.Context(), nzb, []gateStep{stepReturning(&ran, "probe", gate.Result{})})
	if err == nil || err.Error() != "probe returned no result" {
		t.Fatalf("finish error = %v, want probe returned no result", err)
	}
	if got != (ProcessResult{}) {
		t.Fatalf("ProcessResult = %+v, want none", got)
	}
	if nzb.Status != NZBStatusFailed || nzb.FailMessage != "probe returned no result" {
		t.Fatalf("nzb = %q %q, want failed with the no-result cause", nzb.Status, nzb.FailMessage)
	}
	if status := storedStatus(t, u, nzb); status != NZBStatusFailed {
		t.Fatalf("stored status = %q, want failed", status)
	}
}

func TestFinishFirstStepKinds(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		result    gate.Result
		wantRan   []string
		wantErr   bool
		wantGate  bool
		wantState string
	}{
		{"missing", gate.Missing(customerror.ErrUsenetSegmentMissing), []string{"one"}, true, true, NZBStatusFailed},
		{"failed", gate.Failed(boom), []string{"one"}, true, false, NZBStatusFailed},
		{"invalid", gate.Result{}, []string{"one"}, true, false, NZBStatusFailed},
		{"pass", gate.Pass(), []string{"one", "two"}, false, false, NZBStatusCompleted},
		{"unchecked", gate.Unchecked(boom), []string{"one", "two"}, false, false, NZBStatusCompleted},
		{"skipped", gate.Skipped(), []string{"one", "two"}, false, false, NZBStatusCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, _ := gateHarness(t, nil)
			nzb := gateNZB(t, u, gateFile(8))
			var ran []string
			got, err := u.finish(t.Context(), nzb, []gateStep{
				stepReturning(&ran, "one", tc.result),
				stepReturning(&ran, "two", gate.Pass()),
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("finish error = %v, want error %v", err, tc.wantErr)
			}
			if got.GateMissing != tc.wantGate {
				t.Fatalf("GateMissing = %v, want %v", got.GateMissing, tc.wantGate)
			}
			if !slices.Equal(ran, tc.wantRan) {
				t.Fatalf("steps run = %v, want %v", ran, tc.wantRan)
			}
			if status := storedStatus(t, u, nzb); status != tc.wantState {
				t.Fatalf("stored status = %q, want %q", status, tc.wantState)
			}
		})
	}
}

func TestFinishWithNoStepsCompletes(t *testing.T) {
	t.Parallel()
	u, _ := gateHarness(t, nil)
	nzb := gateNZB(t, u, gateFile(8))
	if got, err := u.finish(t.Context(), nzb, nil); err != nil || got != (ProcessResult{}) {
		t.Fatalf("finish = %+v, %v, want completed", got, err)
	}
	if status := storedStatus(t, u, nzb); status != NZBStatusCompleted {
		t.Fatalf("stored status = %q, want completed", status)
	}
}

func TestFinishRecordsNoFailureWhenAMissingStepEndsTheRun(t *testing.T) {
	t.Parallel()
	u, _ := gateHarness(t, nil)
	nzb := gateNZB(t, u, gateFile(8))
	ctx, cancel := context.WithCancel(t.Context())
	steps := []gateStep{{name: "one", run: func() gate.Result {
		cancel()
		return gate.Missing(customerror.ErrUsenetSegmentMissing)
	}}}
	got, err := u.finish(ctx, nzb, steps)
	if err == nil || !got.GateMissing {
		t.Fatalf("finish = %+v, %v, want the missing failure", got, err)
	}
	if status := storedStatus(t, u, nzb); status != NZBStatusParsing {
		t.Fatalf("stored status = %q, want parsing after a cancelled run", status)
	}
}

func TestImportAvailabilitySamplesTheConfiguredPercent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var stated []string
	u, _ := gateHarness(t, func(command, messageID string) string {
		if command != "STAT" {
			return ""
		}
		mu.Lock()
		stated = append(stated, messageID)
		mu.Unlock()
		if messageID == "<seg-3@gate>" {
			return "430 no such article"
		}
		return fmt.Sprintf("223 0 %s", messageID)
	})
	if _, err := u.config.Update(func(cfg *config.Config) error {
		cfg.Usenet.ImportAvailabilitySamplePercent = 25
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	file := storage.NZBFile{Name: "Movie.mkv", FileType: storage.NZBFileTypeMedia}
	for i := range 20 {
		file.Segments = append(file.Segments, storage.NZBSegment{
			Number: i + 1, MessageID: fmt.Sprintf("<seg-%d@gate>", i), Bytes: 1,
		})
	}
	nzb := gateNZB(t, u, file)

	if r := u.checkNZBAvailability(t.Context(), nzb); r.Kind() != gate.KindPass {
		t.Fatalf("checkNZBAvailability = %v, %v, want pass", r.Kind(), r.Err())
	}
	var want []string
	for _, i := range []int{0, 5, 10, 14, 19} {
		want = append(want, fmt.Sprintf("<seg-%d@gate>", i))
	}
	slices.Sort(want)
	mu.Lock()
	defer mu.Unlock()
	got := slices.Clone(stated)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("stat ids = %v, want %v", got, want)
	}
}
