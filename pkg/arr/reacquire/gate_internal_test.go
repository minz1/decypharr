package reacquire

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

type countingGate struct {
	admits, refunds atomic.Int64
	err             error
	// onAdmit runs inside Admit, standing in for another request that gets in while the gate decides.
	onAdmit func()
}

func (g *countingGate) gate() *Gate {
	return &Gate{
		Admit: func() error {
			g.admits.Add(1)
			if g.onAdmit != nil {
				g.onAdmit()
			}
			return g.err
		},
		Refund: func() { g.refunds.Add(1) },
	}
}

func gateService(t *testing.T) (*Service, Binding) {
	t.Helper()
	handler := &waitingReacquireHandler{called: make(chan string, 4)}
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	t.Cleanup(func() { _ = service.Close() })
	binding := Binding{
		ArrName: "radarr", ArrType: arr.Radarr, ArrInstanceFingerprint: testArrInstanceFingerprint,
		EntryID: "entry", EntryFileID: "file", DownloadID: "download", ArrFileID: 40,
		LibraryPath: "/library/movie.mkv", MovieID: 7, Confidence: ConfidenceExactPath,
	}
	if upsertErr := service.UpsertBinding(binding); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	return service, binding
}

func TestGateIsAskedOnlyWhenAJobWouldBeCreated(t *testing.T) {
	t.Parallel()
	service, binding := gateService(t)
	var gate countingGate
	request := Request{EntryID: binding.EntryID, FileID: binding.EntryFileID, Cause: CauseStream, Gate: gate.gate()}

	first, err := service.Reacquire(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Reacquire(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || gate.admits.Load() != 1 || gate.refunds.Load() != 0 {
		t.Fatalf("jobs %q and %q, admits %d, refunds %d; want one job, one admission, no refund",
			first.ID, second.ID, gate.admits.Load(), gate.refunds.Load())
	}
}

func TestAVetoCreatesNoJob(t *testing.T) {
	t.Parallel()
	service, binding := gateService(t)
	vetoed := errors.New("held")
	gate := countingGate{err: vetoed}
	_, err := service.Reacquire(Request{
		EntryID: binding.EntryID, FileID: binding.EntryFileID, Cause: CauseStream, Gate: gate.gate(),
	})
	if !errors.Is(err, vetoed) || len(service.Jobs()) != 0 {
		t.Fatalf("err = %v with %d jobs, want the veto and no job", err, len(service.Jobs()))
	}
}

// If the job cannot be saved after the gate admitted, nothing was created, so the admission is given back.
func TestAFailedSaveRefundsTheAdmission(t *testing.T) {
	t.Parallel()
	service, binding := gateService(t)
	if err := service.jobRepository.Close(); err != nil {
		t.Fatal(err)
	}
	var gate countingGate
	_, err := service.Reacquire(Request{
		EntryID: binding.EntryID, FileID: binding.EntryFileID, Cause: CauseStream, Gate: gate.gate(),
	})
	if err == nil {
		t.Fatal("a job was created although it could not be saved")
	}
	if gate.admits.Load() != 1 || gate.refunds.Load() != 1 || len(service.Jobs()) != 0 {
		t.Fatalf("admits %d refunds %d jobs %d, want the admission refunded and no job",
			gate.admits.Load(), gate.refunds.Load(), len(service.Jobs()))
	}
}

// A job that appears while the gate decides is returned, and the admission is given back.
func TestAJobThatAppearsWhileTheGateDecidesRefundsIt(t *testing.T) {
	t.Parallel()
	service, binding := gateService(t)
	var gate countingGate
	var raced *Job
	gate.onAdmit = func() {
		job, err := service.Reacquire(
			Request{EntryID: binding.EntryID, FileID: binding.EntryFileID, Cause: CauseStream},
		)
		if err != nil {
			t.Error(err)
		}
		raced = job
	}
	job, err := service.Reacquire(Request{
		EntryID: binding.EntryID, FileID: binding.EntryFileID, Cause: CauseStream, Gate: gate.gate(),
	})
	if err != nil || raced == nil || job.ID != raced.ID {
		t.Fatalf("job = %+v err = %v, want the job the racing request created", job, err)
	}
	if gate.admits.Load() != 1 || gate.refunds.Load() != 1 || len(service.Jobs()) != 1 {
		t.Fatalf("admits %d refunds %d jobs %d, want the admission refunded and one job",
			gate.admits.Load(), gate.refunds.Load(), len(service.Jobs()))
	}
}
