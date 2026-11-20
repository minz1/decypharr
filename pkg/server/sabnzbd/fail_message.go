package sabnzbd

import (
	"cmp"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

func sabFailMessage(cause, reason string) string {
	switch cause {
	case grab.CauseArticlesMissing:
		return "Download failed - Not on your server(s) - " + reason
	case grab.CauseSegmentsMissing:
		return "Aborted, cannot be completed - " + reason
	}
	return reason
}

func historyFailMessage(st grab.State, lastError string) string {
	reason := cmp.Or(st.Failure.Reason, lastError)
	if st.Phase != grab.PhaseFailed {
		return reason
	}
	return sabFailMessage(st.Failure.Cause, reason)
}
