package server

import (
	"os"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/server/webdav"
)

// TestMain registers the WebDAV methods once, as cmd/decypharr does at
// startup, before tests build routers in parallel.
func TestMain(m *testing.M) {
	webdav.RegisterMethods()
	os.Exit(m.Run())
}
