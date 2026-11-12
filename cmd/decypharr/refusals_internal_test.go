package decypharr

import (
	"maps"
	"testing"
)

type fixedRefusals map[string]uint64

func (f fixedRefusals) Refusals() map[string]uint64 { return maps.Clone(f) }

// /debug/tasks lists the generation's refused moves next to the manager's.
func TestRefusalsAddTheGenerationToTheManagers(t *testing.T) {
	t.Parallel()
	got := refusals(&generation{refused: 3}, fixedRefusals{"manager": 1, "rclone.mount": 0})()
	want := map[string]uint64{"manager": 1, "rclone.mount": 0, "generation": 3}
	if !maps.Equal(got, want) {
		t.Fatalf("refusals = %v, want %v", got, want)
	}
}
