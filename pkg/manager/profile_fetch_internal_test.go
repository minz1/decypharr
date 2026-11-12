package manager

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// profileClient is a provider whose GetProfile blocks until its ctx ends.
type profileClient struct {
	debrid.Client

	provider string
	calls    atomic.Int32
}

func (c *profileClient) Config() config.Debrid {
	return config.Debrid{Name: c.provider, Provider: c.provider}
}

func (c *profileClient) GetProfile(ctx context.Context) (*types.Profile, error) {
	c.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// The Real-Debrid profile is fetched once per RD client as a work task, and a stop ends it quietly.
func TestRealDebridProfileFetchedOnWork(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		logs := &bytes.Buffer{}
		m := &Manager{
			work:    task.NewRoot(context.WithoutCancel(t.Context()), "work", zerolog.Nop()),
			clients: xsync.NewMap[string, debrid.Client](),
			logger:  zerolog.New(logs),
		}
		rd, tb := &profileClient{provider: "realdebrid"}, &profileClient{provider: "torbox"}
		m.clients.Store("rd", rd)
		m.clients.Store("tb", tb)

		m.startProfileFetches()
		synctest.Wait()
		if rd.calls.Load() != 1 || tb.calls.Load() != 0 {
			t.Fatalf("profile fetches rd=%d torbox=%d, want 1 and 0", rd.calls.Load(), tb.calls.Load())
		}

		ctx, cancel := task.Detached(t.Context(), testStopTimeout)
		defer cancel()
		if err := m.work.Stop(ctx); err != nil {
			t.Fatalf("the work stop waited on the profile fetch: %v", err)
		}
		if strings.Contains(logs.String(), "Failed to get RealDebrid profile") {
			t.Fatalf("a stopped fetch was logged as a failure:\n%s", logs.String())
		}
	})
}
