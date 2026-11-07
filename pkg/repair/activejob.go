package repair

import "github.com/sirrobot01/decypharr/pkg/arr/reacquire"

// HasActiveJob reports whether a reacquisition of the file is already under way. Asking for it again returns that
// job and deletes nothing, so such a request is not a heal and must not be put through Admit: a caller that retries
// would otherwise use up the caps again on every attempt.
func HasActiveJob(jobs []reacquire.Job, entryID, fileID string) bool {
	for _, job := range jobs {
		if job.EntryID == entryID && job.FileID == fileID && !job.Status.Terminal() {
			return true
		}
	}
	return false
}
