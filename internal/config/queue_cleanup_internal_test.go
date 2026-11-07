package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBlocklistLimitsDefaultAndNeverUnlimited(t *testing.T) {
	t.Parallel()
	want := BlocklistLimits{PerBucket: 20, PerArr: 100, Window: time.Hour}
	if got := (QueueCleanup{}).BlocklistLimits(); got != want {
		t.Fatalf("zero limits = %+v, want %+v", got, want)
	}
	bad := QueueCleanup{BlocklistPerBucket: -1, BlocklistPerArr: -9, BlocklistWindow: "later"}
	if got := bad.BlocklistLimits(); got != want {
		t.Fatalf("invalid limits = %+v, want the defaults %+v", got, want)
	}
	set := QueueCleanup{BlocklistPerBucket: 5, BlocklistPerArr: 8, BlocklistWindow: "30m"}
	if got := set.BlocklistLimits(); got != (BlocklistLimits{PerBucket: 5, PerArr: 8, Window: 30 * time.Minute}) {
		t.Fatalf("configured limits = %+v", got)
	}
}

func TestBlocklistLimitKeysAreOmittedWhenUnset(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(QueueCleanup{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("an untouched queue_cleanup serializes %s; the new keys must be omitted", raw)
	}
}
