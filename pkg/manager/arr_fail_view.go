package manager

import (
	"time"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

const jobPrunedStatus = "job pruned"

type ArrFailView struct {
	State     string    `json:"state"`
	Arr       string    `json:"arr"`
	Waiting   string    `json:"waiting,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	NextRetry time.Time `json:"next_retry,omitzero"`
	JobStatus string    `json:"job_status,omitempty"`
}

func (m *Manager) ArrFailView(g GrabView) (ArrFailView, bool) {
	a := g.State.ArrFail
	if a.State == grab.ArrFailNone {
		return ArrFailView{}, false
	}
	v := ArrFailView{State: a.State.String(), Arr: a.Arr}
	switch a.State {
	case grab.ArrFailPending:
		if m.failer != nil {
			if st, ok := m.failer.status(g.State.Key, g.State.Failure.At); ok {
				v.Waiting, v.LastError, v.NextRetry = st.Waiting, st.LastError, st.NextRetry
			}
		}
	case grab.ArrFailQueued:
		if rec := m.recoveryService(); rec != nil && a.JobID != "" {
			v.JobStatus = jobPrunedStatus
			if job, ok := rec.JobByID(a.JobID); ok {
				v.JobStatus = string(job.Status)
			}
		}
	case grab.ArrFailNone, grab.ArrFailSkipped:
	}
	return v, true
}
