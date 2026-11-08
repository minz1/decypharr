package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// /debug/tasks sits behind the same auth as the other debug routes.
func TestDebugTasksListsRunningTasksBehindAuth(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.UseAuth = true
		cfg.Auth = &config.Auth{TokenOnly: true, APIToken: "debug-token"}
	})
	server := New(t.Context(), mgr, store, logger.Discard(), nil)
	server.SetTaskSnapshot(func() []task.Info {
		return []task.Info{{Path: "generation/http:serve"}}
	})

	anonymous := httptest.NewRecorder()
	server.router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/debug/tasks", nil))
	if anonymous.Code == http.StatusOK {
		t.Fatal("/debug/tasks answered without credentials")
	}

	request := httptest.NewRequest(http.MethodGet, "/debug/tasks", nil)
	request.Header.Set("Authorization", "Bearer debug-token")
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", response.Code, response.Body.String())
	}
	var body struct {
		Tasks []struct {
			Path    string `json:"path"`
			Started string `json:"started"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 1 || body.Tasks[0].Path != "generation/http:serve" {
		t.Fatalf("tasks = %+v, want the one snapshot entry", body.Tasks)
	}
}
