package manager

import (
	"fmt"
	"time"

	"github.com/sirrobot01/decypharr/internal/state"
)

type failItemPhase uint8

const (
	failWaiting failItemPhase = iota
	failAckWaiting
	failRunning
)

func (p failItemPhase) String() string {
	switch p {
	case failWaiting:
		return "waiting"
	case failAckWaiting:
		return "ack_waiting"
	case failRunning:
		return "running"
	}
	return fmt.Sprintf("fail_item_phase(%d)", uint8(p))
}

func failItemSuccessors(p failItemPhase) []failItemPhase {
	switch p {
	case failWaiting, failAckWaiting:
		return []failItemPhase{failRunning}
	case failRunning:
		return []failItemPhase{failWaiting, failAckWaiting}
	}
	return nil
}

type failWait uint8

const (
	failWaitNone failWait = iota
	failWaitArr
	failWaitKey
)

type failItem struct {
	key, arr, provider string
	at                 time.Time
	recheck            bool
	phase              failItemPhase
	attempt            uint64
	fails              int
	due                time.Time
	wait               failWait
	lastErr            string
	ackSince           time.Time
	ackWarned          bool
	jobID              string
	superseded         bool
	untold             bool
}

type transitionRefusedError struct{ from, to failItemPhase }

func (e *transitionRefusedError) Error() string {
	return fmt.Sprintf("fail item %s -> %s is not a move", e.from, e.to)
}

func (it *failItem) move(to failItemPhase) error {
	if !state.Legal(failItemSuccessors, it.phase, to) {
		return &transitionRefusedError{from: it.phase, to: to}
	}
	it.phase = to
	return nil
}

func (it *failItem) bucket() string { return failBucket(it.arr, it.provider) }

func failBucket(arrName, provider string) string { return arrName + "/" + provider }
