package realdebrid

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/ratelimit"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// countingLimiter counts the requests that reach the rate limiter, the first step of every request, and holds each
// one there until the bubble ends, so none reaches the network.
type countingLimiter struct {
	takes atomic.Int32
	hold  chan struct{}
}

func (l *countingLimiter) Take() time.Time {
	l.takes.Add(1)
	<-l.hold
	return time.Now()
}

// New makes no request: the profile is fetched later, on the manager's work, where a stop can end it.
func TestNewMakesNoRequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		limiter := &countingLimiter{hold: make(chan struct{})}
		limits := map[string]ratelimit.Limiter{"main": limiter, "repair": limiter, "download": limiter}
		if _, err := New(
			config.Debrid{Name: "rd", Provider: "realdebrid", APIKey: "key"},
			limits,
			types.ProviderOptions{Logger: zerolog.Nop()},
		); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := limiter.takes.Load(); got != 0 {
			t.Errorf("New made %d request(s), want none", got)
		}
	})
}
