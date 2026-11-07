package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

type debugBody struct {
	Tasks  []map[string]any `json:"tasks"`
	Actors []struct {
		Name  string `json:"name"`
		State string `json:"state"`
		Keys  []struct {
			Key     string `json:"key"`
			Depth   int    `json:"depth"`
			Running bool   `json:"running"`
			Timers  int    `json:"timers"`
		} `json:"keys"`
	} `json:"actors"`
}

func getDebugTasks(t *testing.T, server *Server) debugBody {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/debug/tasks", nil)
	request.Header.Set("Authorization", "Bearer debug-token")
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", response.Code, response.Body.String())
	}
	var body debugBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, response.Body.String())
	}
	return body
}

func debugServer(t *testing.T) *Server {
	t.Helper()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.UseAuth = true
		cfg.Auth = &config.Auth{TokenOnly: true, APIToken: "debug-token"}
	})
	return New(mgr, store, logger.Discard())
}

// The actors field is additive: with no runner wired it is an empty list next to the tasks.
func TestDebugTasksHasAnEmptyActorsListByDefault(t *testing.T) {
	t.Parallel()
	body := getDebugTasks(t, debugServer(t))

	if body.Actors == nil || len(body.Actors) != 0 {
		t.Fatalf("actors = %+v, want an empty list", body.Actors)
	}
	if body.Tasks == nil {
		t.Fatal("the tasks field went missing")
	}
}

// /debug/tasks shows each runner's per-key mailbox depth, next to the task tree.
func TestDebugTasksShowsTheMailboxesOfTheWiredRunners(t *testing.T) {
	t.Parallel()
	server := debugServer(t)
	g := newGroup(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	h := keyed.HandlerFunc[string, int](func(_ context.Context, _ string, _ int, _ *keyed.Effects[string, int]) {
		entered <- struct{}{}
		<-release
	})
	runner := keyed.New[string, int, struct{}](g, "entries", h, keyed.Options[int]{})
	t.Cleanup(func() { close(release) })
	for range 3 {
		if err := runner.Send("Show.S01", 1); err != nil {
			t.Fatal(err)
		}
	}
	<-entered // one message is being handled, two wait
	server.SetActorSnapshot(func() []keyed.Info { return []keyed.Info{runner.Snapshot()} })

	body := getDebugTasks(t, server)

	if len(body.Actors) != 1 || body.Actors[0].Name != "entries" || body.Actors[0].State != "running" {
		t.Fatalf("actors = %+v, want the one running runner", body.Actors)
	}
	keys := body.Actors[0].Keys
	if len(keys) != 1 || keys[0].Key != "Show.S01" || keys[0].Depth != 2 || !keys[0].Running {
		t.Fatalf("keys = %+v, want Show.S01 with two queued messages, running", keys)
	}
}
