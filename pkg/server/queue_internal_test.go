package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/server/qbit"
	"github.com/sirrobot01/decypharr/pkg/server/sabnzbd"
)

func TestQueueReadFailuresReachHTTPClients(t *testing.T) {
	t.Parallel()
	store := managertest.Store(t, func(cfg *config.Config) { cfg.UseAuth = false })
	mgr, err := manager.New(store, logger.Discard())
	if err != nil {
		t.Fatal(err)
	}
	// Stop fails on the storage closed below; that is the point of the test.
	t.Cleanup(func() { _ = mgr.Stop() })
	server := &Server{manager: mgr, config: store}
	qbitRoutes := qbit.New(mgr, store, zerolog.Nop()).Routes()
	sabRoutes := sabnzbd.New(mgr, store, zerolog.Nop()).Routes()
	if closeErr := mgr.Storage().Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	for _, tc := range []struct {
		name, path string
		handler    http.Handler
	}{
		{"web queue", "/api/torrents", http.HandlerFunc(server.handleGetTorrents)},
		{"qbit queue", "/torrents/info", qbitRoutes},
		{"sab queue", "/api/?mode=queue", sabRoutes},
		{"sab history", "/api/?mode=history", sabRoutes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			tc.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
			}
		})
	}
}
