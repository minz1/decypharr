package gate_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/usenet/internal/gate"
)

func TestResultKindsAndErrors(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	var zero gate.Result
	for _, tc := range []struct {
		name     string
		got      gate.Result
		kind     gate.Kind
		wantErr  bool
		wantWrap error
	}{
		{name: "zero value", got: zero, kind: gate.KindInvalid, wantErr: true},
		{name: "pass", got: gate.Pass(), kind: gate.KindPass},
		{name: "missing", got: gate.Missing(cause), kind: gate.KindMissing, wantErr: true, wantWrap: cause},
		{name: "missing without a cause", got: gate.Missing(nil), kind: gate.KindMissing, wantErr: true},
		{name: "failed", got: gate.Failed(cause), kind: gate.KindFailed, wantErr: true, wantWrap: cause},
		{name: "failed without a cause", got: gate.Failed(nil), kind: gate.KindFailed, wantErr: true},
		{name: "unchecked", got: gate.Unchecked(cause), kind: gate.KindUnchecked, wantErr: true, wantWrap: cause},
		{name: "unchecked without a cause", got: gate.Unchecked(nil), kind: gate.KindUnchecked, wantErr: true},
		{name: "skipped", got: gate.Skipped(), kind: gate.KindSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.got.Kind(); got != tc.kind {
				t.Errorf("Kind = %v, want %v", got, tc.kind)
			}
			err := tc.got.Err()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantWrap != nil && !errors.Is(err, tc.wantWrap) {
				t.Errorf("Err = %v, want it to wrap %v", err, tc.wantWrap)
			}
			if tc.wantWrap == nil && err != nil && errors.Is(err, cause) {
				t.Errorf("Err = %v, must not carry an unrelated cause", err)
			}
		})
	}
}

func TestKindsAreDistinct(t *testing.T) {
	t.Parallel()
	var seen []gate.Kind
	for _, kind := range []gate.Kind{
		gate.KindInvalid, gate.KindPass, gate.KindMissing,
		gate.KindFailed, gate.KindUnchecked, gate.KindSkipped,
	} {
		if slices.Contains(seen, kind) {
			t.Fatalf("kind %v repeated", kind)
		}
		seen = append(seen, kind)
	}
	if gate.KindInvalid != 0 {
		t.Fatal("the zero Kind must be Invalid")
	}
}
