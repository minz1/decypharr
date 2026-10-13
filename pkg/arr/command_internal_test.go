package arr

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommandStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/command/7":
			_, _ = w.Write([]byte(`{"id":7,"status":"failed","message":"Import failed"}`))
		case "/api/v3/command/8":
			http.Error(w, "no", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	s := testService(Arr{Host: server.URL, Token: "secret", Type: Sonarr})

	command, found, err := s.CommandStatus(t.Context(), "arr", 7)
	if err != nil || !found || command.Status != "failed" || command.Message != "Import failed" {
		t.Fatalf("command 7 = %+v, %v, %v", command, found, err)
	}
	if _, found, err = s.CommandStatus(t.Context(), "arr", 8); err == nil || found {
		t.Fatalf("command 8: found=%v err=%v, want an error", found, err)
	}
	if _, found, err = s.CommandStatus(t.Context(), "arr", 9); err != nil || found {
		t.Fatalf("command 9: found=%v err=%v, want not found", found, err)
	}
}
