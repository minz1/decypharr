package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

type saveReply struct {
	Restarted bool     `json:"restarted"`
	Restart   string   `json:"restart"`
	Warnings  []string `json:"warnings"`

	RestartError string `json:"restart_error"`
}

func saveConfig(t *testing.T, server *Server, body string) (*httptest.ResponseRecorder, saveReply) {
	t.Helper()
	response := httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	var reply saveReply
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
		t.Fatalf("status %d, body %q: %v", response.Code, response.Body.String(), err)
	}
	return response, reply
}

func newRestartServer(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	mgr, store := managertest.New(t, nil)
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
	var requests atomic.Int32
	server.SetRestartFunc(func() { requests.Add(1) })
	return server, &requests
}

// A save that needs a restart answers 202 and has already asked the
// generation for it when the handler returns: no goroutine is left behind.
func TestColdConfigSaveRequestsRestartAndReturns202(t *testing.T) {
	t.Parallel()
	server, requests := newRestartServer(t)
	response, reply := saveConfig(t, server, `{"port":"9191"}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	if !reply.Restarted || reply.Restart != "pending" {
		t.Fatalf("reply = %+v, want restarted and restart=pending", reply)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("restart requests = %d when the handler returned, want 1", got)
	}
}

// A hot-only save on top of an unapplied cold one is still a pending restart.
func TestHotSaveAfterColdSaveStillReportsPendingRestart(t *testing.T) {
	t.Parallel()
	server, requests := newRestartServer(t)
	saveConfig(t, server, `{"port":"9191"}`)
	response, reply := saveConfig(t, server, `{"app_url":"https://example.test"}`)
	if response.Code != http.StatusAccepted || !reply.Restarted || reply.Restart != "pending" {
		t.Fatalf("status = %d, reply = %+v; want 202 with the restart still pending", response.Code, reply)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("restart requests = %d, want one per save (the generation makes them idempotent)", got)
	}
}

func TestHotConfigSaveStaysOKWithoutRestart(t *testing.T) {
	t.Parallel()
	server, requests := newRestartServer(t)
	response, reply := saveConfig(t, server, `{"app_url":"https://example.test"}`)
	if response.Code != http.StatusOK || reply.Restarted || reply.Restart != "none" {
		t.Fatalf("status = %d, reply = %+v; want 200 and restart=none", response.Code, reply)
	}
	if requests.Load() != 0 {
		t.Fatal("a hot save requested a restart")
	}
}

// With no generation to ask, the save is kept, the failure is reported, and the
// next save asks again.
func TestRestartThatCannotBeRequestedIsReportedAndRetried(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	server := &Server{manager: mgr, config: store, logger: zerolog.Nop()}
	response, reply := saveConfig(t, server, `{"port":"9191"}`)
	if response.Code != http.StatusOK || reply.Restarted || reply.Restart != "failed" || reply.RestartError == "" {
		t.Fatalf("status = %d, reply = %+v; want 200, restart=failed and its cause", response.Code, reply)
	}
	if store.Pending() != config.PendingFailed {
		t.Fatalf("pending = %v, want failed", store.Pending())
	}

	var requests atomic.Int32
	server.SetRestartFunc(func() { requests.Add(1) })
	response, reply = saveConfig(t, server, `{"app_url":"https://example.test"}`)
	if response.Code != http.StatusAccepted || reply.Restart != "pending" || requests.Load() != 1 {
		t.Fatalf("retry: status = %d, reply = %+v, requests = %d; want 202, pending and one request",
			response.Code, reply, requests.Load())
	}
}

func TestGetConfigReportsRestartState(t *testing.T) {
	t.Parallel()
	server, _ := newRestartServer(t)
	read := func() string {
		response := httptest.NewRecorder()
		server.handleGetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		var body struct {
			Restart string `json:"restart"`
			Port    string `json:"port"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Port == "" {
			t.Fatal("existing config fields are missing from the response")
		}
		return body.Restart
	}
	if got := read(); got != "none" {
		t.Fatalf("restart = %q, want none", got)
	}
	saveConfig(t, server, `{"port":"9191"}`)
	if got := read(); got != "pending" {
		t.Fatalf("restart = %q after a cold save, want pending", got)
	}
}

func TestSetupCompleteReturns202AndRequestsRestart(t *testing.T) {
	t.Parallel()
	store := managertest.Store(t, nil)
	server := newTestServer(t, store)
	var requests atomic.Int32
	server.SetRestartFunc(func() { requests.Add(1) })
	body := `{"auth":{"username":"admin","password":"secret"},"debrid":{"provider":"realdebrid","api_key":"k"},` +
		`"download":{"download_folder":"` + t.TempDir() + `"}}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/setup/complete", strings.NewReader(body))
	server.setupCompleteHandler(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", response.Code, response.Body.String())
	}
	var reply SetupWizardResponse
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || !reply.Success {
		t.Fatalf("body = %q (%v), want the success reply", response.Body.String(), err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("restart requests = %d, want 1", got)
	}
}

func TestRestartWithoutAGenerationReportsAnError(t *testing.T) {
	t.Parallel()
	server := &Server{logger: zerolog.Nop()}
	if err := server.Restart(); err == nil {
		t.Fatal("Restart succeeded with no restart function")
	}
}
