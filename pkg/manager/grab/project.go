package grab

// Projection is how every reader of the download queue shows one grab.
type Projection struct {
	// Client is set for the phases qBit and SAB list.
	Client bool
	// Dashboard is set for the phases /api/torrents lists.
	Dashboard bool
	QBitState string
	SABStatus string
	SABFile   string
	// Label is the dashboard text.
	Label    string
	Progress float64
}

const (
	qbitQueued      = "queuedDL"
	qbitDownloading = "downloading"
	qbitComplete    = "pausedUP"
	qbitError       = "error"
)

// Project maps a grab state and its stored progress (0..1) to what the qBit, SAB and dashboard readers show.
func Project(st State, progress float64) Projection {
	listed := func(qbit, sabStatus, sabFile, label string, p float64) Projection {
		return Projection{
			Client: true, Dashboard: true, QBitState: qbit, SABStatus: sabStatus, SABFile: sabFile,
			Label: label, Progress: p,
		}
	}
	switch st.Phase {
	case PhaseAbsent:
		return Projection{}
	case PhaseQueued:
		return listed(qbitQueued, "Queued", "queued", "Queued", 0)
	case PhaseThrottled:
		return listed(qbitQueued, "Queued", "queued", "Waiting", 0)
	case PhaseSubmitting:
		return listed(qbitQueued, "Queued", "queued", "Submitting", 0)
	case PhaseProvider:
		return listed(qbitDownloading, "Downloading", "active", "Downloading", progress)
	case PhaseAwaitingLinks:
		return listed(qbitDownloading, "Downloading", "active", "Waiting for links", 1)
	case PhaseActing:
		if progress <= 0 {
			progress = 1
		}
		return listed(qbitDownloading, "Downloading", "active", "Linking", progress)
	case PhaseComplete:
		return listed(qbitComplete, "Completed", "finished", "Completed", 1)
	case PhaseFailed:
		return listed(qbitError, "Failed", "failed", "Failed", progress)
	case PhaseDeleting:
		return Projection{Dashboard: true, Label: "Deleting"}
	}
	return Projection{}
}
