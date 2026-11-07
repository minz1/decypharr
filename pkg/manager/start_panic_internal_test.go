package manager

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A panic in a service's start is reported as that service's start failure. Without it the start sequence waits for
// a result that never comes and the manager stays Starting for good.
func TestPanickingServiceStartFailsTheStep(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	svc, err := m.superviseService("panicky", func(context.Context) error { panic("boom") }, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- svc.run(t.Context()) }()

	select {
	case err = <-result:
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("start result = %v, want an error naming the panic", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start step never finished after a panic in the service's start")
	}
}
