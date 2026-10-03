package webdav

import (
	"os"
	"testing"
)

// TestMain registers the WebDAV methods once, as cmd/decypharr does at
// startup, before tests build routers in parallel.
func TestMain(m *testing.M) {
	RegisterMethods()
	os.Exit(m.Run())
}
