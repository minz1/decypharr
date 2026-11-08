package decypharr

import (
	"sync"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/stats"
)

// shareStates holds the NFS and SMB task groups of one generation so the stats
// collector can report their run state. startShares sets a group when it starts
// the server; a server that was never started has no group and reads stopped.
type shareStates struct {
	mu  sync.Mutex
	nfs *task.Group
	smb *task.Group
}

func (s *shareStates) setNFS(g *task.Group) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nfs = g
}

func (s *shareStates) setSMB(g *task.Group) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.smb = g
}

// accessState returns the read-only status func handed to the stats collector.
func (s *shareStates) accessState() stats.AccessState {
	return func() (string, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return groupState(s.nfs), groupState(s.smb)
	}
}

// groupState maps a share group's lifecycle state to an access state. A nil
// group, and a group that is stopping or stopped, is stopped.
func groupState(g *task.Group) string {
	if g == nil {
		return stats.ShareStopped
	}
	switch g.State() {
	case task.Running:
		return stats.ShareRunning
	case task.Failing, task.Failed:
		return stats.ShareFailed
	case task.Stopping, task.Stopped:
		return stats.ShareStopped
	}
	return stats.ShareStopped
}
