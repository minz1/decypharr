package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/repair"
)

// tripUnclassified makes the repair breaker hold the bucket a manual reacquisition of an unknown entry falls in,
// by confirming ten distinct failures there, as automated paths do.
func tripUnclassified(t *testing.T, service *repair.Service) {
	t.Helper()
	var err error
	for i := range 10 {
		err = service.Admit(t.Context(), repair.HealRequest{
			Provider: service.ProviderFor("entry-1"), Class: repair.ClassOther, Subject: fmt.Sprintf("failing-%d", i),
		})
	}
	if !errors.Is(err, repair.ErrHealHeld) {
		t.Fatalf("setup: the bucket did not trip: %v", err)
	}
}

// A manual reacquisition deletes an Arr file, so it goes through the repair breaker like any other heal, but it is
// no evidence of a failure: any number of them never trip the breaker.
func TestManualReacquireIsHeldByATrippedBreakerUnlessForcedAndNeverTripsIt(t *testing.T) {
	t.Parallel()
	mgr, _ := managertest.New(t, nil)
	s := &Server{manager: mgr, logger: zerolog.Nop()}
	admit := func(i int, force bool) int {
		w := httptest.NewRecorder()
		request := reacquire.Request{EntryID: fmt.Sprintf("entry-%d", i), FileID: "file"}
		if s.admitReacquire(w, httptest.NewRequest(http.MethodPost, "/api/arr/reacquire", nil), request, force, false) {
			return http.StatusAccepted
		}
		return w.Code
	}
	for i := range 12 {
		if got := admit(i, false); got != http.StatusAccepted {
			t.Fatalf("manual reacquire %d = %d, want accepted: operators never trip the breaker", i, got)
		}
	}
	if status := mgr.Repair().BreakerStatus(); len(status) != 0 {
		t.Fatalf("manual reacquires tripped %+v", status)
	}
	tripUnclassified(t, mgr.Repair())
	if got := admit(20, false); got != http.StatusConflict {
		t.Fatalf("manual reacquire in a tripped bucket = %d, want the breaker's conflict", got)
	}
	if got := admit(21, true); got != http.StatusAccepted {
		t.Fatalf("forced reacquire = %d, want accepted", got)
	}
}

// Asking again for a file that already has a job returns that job; it deletes nothing, so it is not a heal to
// count and a tripped bucket does not refuse it.
func TestManualReacquireOfAFileWithAJobIsNotCounted(t *testing.T) {
	t.Parallel()
	mgr, _ := managertest.New(t, nil)
	s := &Server{manager: mgr, logger: zerolog.Nop()}
	request := reacquire.Request{EntryID: "entry-1", FileID: "file"}
	tripUnclassified(t, mgr.Repair())
	post := func(active bool) bool {
		return s.admitReacquire(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/", nil),
			request,
			false,
			active,
		)
	}
	if post(false) {
		t.Fatal("a new reacquisition passed a tripped bucket")
	}
	if !post(true) {
		t.Fatal("a repeated request for a file with a job was held")
	}
	jobs := []reacquire.Job{{EntryID: "entry-1", FileID: "file", Status: reacquire.StatusQueued}}
	if !repair.HasActiveJob(jobs, "entry-1", "file") || repair.HasActiveJob(jobs, "entry-1", "other-file") {
		t.Fatal("HasActiveJob does not match on entry and file")
	}
}

func TestValidateRepairConfigChecksTheBreakerKeysEvenWhenDisabled(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]config.RepairConfig{
		"bad duration":   {ConfirmGap: "soon"},
		"zero duration":  {CanaryCache: "0s"},
		"negative count": {HealCapPerRun: -1},
	} {
		if validateRepairConfig(&cfg) == "" {
			t.Errorf("%s accepted", name)
		}
	}
	if msg := validateRepairConfig(&config.RepairConfig{ConfirmGap: "2h", HealCapPerRun: 10}); msg != "" {
		t.Errorf("valid breaker keys rejected: %s", msg)
	}
	if msg := validateRepairConfig(&config.RepairConfig{}); msg != "" {
		t.Errorf("the zero config rejected: %s", msg)
	}
}

func TestWebhookRecheckNeverCarriesForce(t *testing.T) {
	t.Parallel()
	opts := webhookRecheckOptions(true, "POST /webhooks/tautulli")
	if !opts.Fix || opts.Source != repair.SourceWebhook || opts.Force {
		t.Fatalf("options = %+v, want a fixing webhook without force", opts)
	}
}
