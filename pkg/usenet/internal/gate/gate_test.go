package gate

import (
	"errors"
	"testing"
)

func TestResultKindsAndErrors(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	var zero Result
	for _, tc := range []struct {
		name string
		got  Result
		kind Kind
		err  error
	}{
		{name: "zero value", got: zero, kind: KindInvalid, err: errNoResult},
		{name: "pass", got: Pass(), kind: KindPass},
		{name: "missing", got: Missing(cause), kind: KindMissing, err: cause},
		{name: "missing without a cause", got: Missing(nil), kind: KindMissing, err: errNoCause},
		{name: "failed", got: Failed(cause), kind: KindFailed, err: cause},
		{name: "failed without a cause", got: Failed(nil), kind: KindFailed, err: errNoCause},
		{name: "unchecked", got: Unchecked(cause), kind: KindUnchecked, err: cause},
		{name: "unchecked without a cause", got: Unchecked(nil), kind: KindUnchecked, err: errNoCause},
		{name: "skipped", got: Skipped(), kind: KindSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.got.Kind(); got != tc.kind {
				t.Errorf("Kind = %v, want %v", got, tc.kind)
			}
			if got := tc.got.Err(); got != tc.err {
				t.Errorf("Err = %v, want %v", got, tc.err)
			}
		})
	}
}

func TestKindsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[Kind]bool{}
	for _, kind := range []Kind{KindInvalid, KindPass, KindMissing, KindFailed, KindUnchecked, KindSkipped} {
		if seen[kind] {
			t.Fatalf("kind %v repeated", kind)
		}
		seen[kind] = true
	}
	if KindInvalid != 0 {
		t.Fatal("the zero Kind must be Invalid")
	}
}
