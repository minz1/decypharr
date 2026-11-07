package manager

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

// Between the group's context ending and the closer task running, the queue is not yet marked closed. A job
// submitted in that window would be held for ever, so Submit also checks the context.
func TestSubmitRefusesOnceTheGroupContextEnded(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(t.Context())
	q := &JobQueue{
		held:   map[string]int{},
		logger: zerolog.Nop(),
		group:  task.NewRoot(parent, "test", zerolog.Nop()),
	}
	q.cond = sync.NewCond(&q.mu)

	cancel()

	if err := q.Submit(&Job{ID: "late"}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("Submit = %v, want ErrStopped", err)
	}
	if q.Holds("late") || q.Len() != 0 {
		t.Fatal("the refused job was kept")
	}
}
