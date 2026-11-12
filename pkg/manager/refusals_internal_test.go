package manager

import (
	"maps"
	"testing"

	"github.com/rs/zerolog"
)

type refusingMount struct {
	MountManager
}

func (refusingMount) Refusals() map[string]uint64 { return map[string]uint64{"rclone.mount": 2} }

// Refusals counts each machine's refused moves and folds in the mount's.
func TestRefusalsCountsEachMachineAndTheMount(t *testing.T) {
	t.Parallel()
	m := &Manager{life: newLifecycle(), mountGate: NewMountGate(zerolog.Nop()), mountManager: refusingMount{}}
	m.life.Transition(lifecycleStopped, lifecycleStarting)
	m.mountGate.transition(mountGateStopping, mountGateServing)
	m.mountGate.transition(mountGateStopping, mountGateServing)

	got := m.Refusals()
	want := map[string]uint64{
		"manager": 1, "mount_gate": 2, "repair_breaker": 0, "arr_queue_guard": 0, "rclone.mount": 2,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("Refusals() = %v, want %v", got, want)
	}
}
