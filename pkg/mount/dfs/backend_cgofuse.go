//go:build !(linux || (darwin && amd64))

package dfs

import (
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend/cgofuse"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
)

func newBackend(
	_ backend.Type,
	v *vfs.Manager,
	c *config.FuseConfig,
	logs *logger.Factory,
) (backend.Backend, error) {
	return cgofuse.NewBackend(v, c, logs.New("cgofuse"))
}
