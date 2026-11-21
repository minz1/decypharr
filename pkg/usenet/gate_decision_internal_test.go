package usenet

import (
	"context"
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/internal/gate"
)

func oneFileNZB() *storage.NZB {
	file := gateFile(len(mkvPayload()))
	return &storage.NZB{ID: "gate", Files: []storage.NZBFile{file}}
}

func TestFoldsPassKindsThroughUnchanged(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	for _, tc := range []struct {
		name  string
		fold  func(u *Usenet, check func(*storage.NZBFile) gate.Result) gate.Result
		given gate.Result
		want  gate.Kind
	}{
		{"availability: invalid", availabilityFold, gate.Result{}, gate.KindInvalid},
		{"availability: failed", availabilityFold, gate.Failed(cause), gate.KindFailed},
		{"availability: skipped", availabilityFold, gate.Skipped(), gate.KindSkipped},
		{"content: invalid", contentFold, gate.Result{}, gate.KindInvalid},
		{"content: skipped", contentFold, gate.Skipped(), gate.KindSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, _ := gateHarness(t, nil)
			got := tc.fold(u, func(*storage.NZBFile) gate.Result { return tc.given })
			if got.Kind() != tc.want || !errors.Is(got.Err(), tc.given.Err()) {
				t.Fatalf("fold = %v (%v), want %v unchanged", got.Kind(), got.Err(), tc.want)
			}
		})
	}
}

func availabilityFold(u *Usenet, check func(*storage.NZBFile) gate.Result) gate.Result {
	return u.foldAvailability(context.Background(), oneFileNZB(), check)
}

func contentFold(u *Usenet, check func(*storage.NZBFile) gate.Result) gate.Result {
	return u.foldContent(context.Background(), oneFileNZB(), check)
}

func TestZeroResultFromAFoldFailsFinishClosed(t *testing.T) {
	t.Parallel()
	var zero gate.Result
	for _, tc := range []struct {
		name string
		gate string
		fold func(u *Usenet, check func(*storage.NZBFile) gate.Result) gate.Result
		want string
	}{
		{"availability", "availability check", availabilityFold, "availability check returned no result"},
		{"content", "content verification", contentFold, "content verification returned no result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, _ := gateHarness(t, nil)
			folded := tc.fold(u, func(*storage.NZBFile) gate.Result { return zero })
			result, cause, failure := gateOutcome(tc.gate, folded)
			if failure == nil || failure.Error() != tc.want {
				t.Fatalf("failure = %v, want %q", failure, tc.want)
			}
			if cause == nil || cause.Error() != tc.want {
				t.Fatalf("stored cause = %v, want %q", cause, tc.want)
			}
			if result != (ProcessResult{}) {
				t.Fatalf("ProcessResult = %+v, want GateMissing false", result)
			}
		})
	}
}

func TestGateOutcomeMapsEveryKind(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	for _, tc := range []struct {
		name    string
		given   gate.Result
		result  ProcessResult
		stored  string
		failure string
	}{
		{"missing", gate.Missing(cause), ProcessResult{GateMissing: true}, "cause", "step failed: cause"},
		{"failed", gate.Failed(cause), ProcessResult{}, "cause", "step failed: cause"},
		{"pass", gate.Pass(), ProcessResult{}, "", ""},
		{"unchecked", gate.Unchecked(cause), ProcessResult{}, "", ""},
		{"skipped", gate.Skipped(), ProcessResult{}, "", ""},
		{"invalid", gate.Result{}, ProcessResult{}, "step returned no result", "step returned no result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, stored, failure := gateOutcome("step", tc.given)
			if result != tc.result {
				t.Errorf("ProcessResult = %+v, want %+v", result, tc.result)
			}
			if got := errText(stored); got != tc.stored {
				t.Errorf("stored = %q, want %q", got, tc.stored)
			}
			if got := errText(failure); got != tc.failure {
				t.Errorf("failure = %q, want %q", got, tc.failure)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRepairErrorMapsEveryKind(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	var zero gate.Result
	for _, tc := range []struct {
		name      string
		given     gate.Result
		surface   bool
		want      error
		wantError bool
	}{
		{"missing", gate.Missing(cause), false, cause, true},
		{"failed", gate.Failed(cause), false, cause, true},
		{"unchecked for availability", gate.Unchecked(cause), false, nil, false},
		{"unchecked for the head check", gate.Unchecked(cause), true, cause, true},
		{"pass", gate.Pass(), true, nil, false},
		{"skipped", gate.Skipped(), true, nil, false},
		{"invalid for availability", zero, false, nil, true},
		{"invalid for the head check", zero, true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := repairError(tc.given, tc.surface)
			if (err != nil) != tc.wantError {
				t.Fatalf("repairError = %v, want error %v", err, tc.wantError)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("repairError = %v, want %v", err, tc.want)
			}
			if tc.given.Kind() == gate.KindInvalid {
				for _, sentinel := range neitherSentinel() {
					if errors.Is(err, sentinel) {
						t.Fatalf("repairError(invalid) = %v, must not match %v", err, sentinel)
					}
				}
			}
		})
	}
}

func TestCheckWholeFileMapsThroughTheRepairError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reply  func(command, messageID string) string
		served bool
		want   error
	}{
		{name: "pass", served: true},
		{name: "missing", served: false, want: customerror.ErrUsenetSegmentMissing},
		{name: "unchecked", reply: statDownReply, served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			storeSegmented(t, rig, 3, func(int) bool { return tc.served })
			if err := rig.u.CheckWholeFile(t.Context(), "gate", "Movie.mkv"); !errors.Is(err, tc.want) {
				t.Fatalf("CheckWholeFile = %v, want %v", err, tc.want)
			}
		})
	}
}
