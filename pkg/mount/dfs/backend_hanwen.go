//go:build linux || (darwin && amd64)

package dfs

import (
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend/cgofuse"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend/hanwen"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
)

func newBackend(
	t backend.Type,
	v *vfs.Manager,
	c *config.FuseConfig,
	logs *logger.Factory,
) (backend.Backend, error) {
	if t == backend.Hanwen {
		return hanwen.NewBackend(v, c, logs.New("hanwen-backend"))
	}
	return cgofuse.NewBackend(v, c, logs.New("cgofuse"))
}
