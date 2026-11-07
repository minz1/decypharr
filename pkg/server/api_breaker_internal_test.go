package server

import (
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

// A manual reacquisition deletes an Arr file, so it goes through the repair breaker like any other heal.
func TestManualReacquireIsHeldByATrippedBreakerUnlessForced(t *testing.T) {
	t.Parallel()
	mgr, _ := managertest.New(t, nil)
	s := &Server{manager: mgr, logger: zerolog.Nop()}
	admit := func(i int, force bool) int {
		w := httptest.NewRecorder()
		request := reacquire.Request{EntryID: fmt.Sprintf("entry-%d", i), FileID: "file"}
		if s.admitReacquire(w, httptest.NewRequest(http.MethodPost, "/api/arr/reacquire", nil), request, force) {
			return http.StatusAccepted
		}
		return w.Code
	}

	for i := range 9 {
		if got := admit(i, false); got != http.StatusAccepted {
			t.Fatalf("reacquire %d = %d, want accepted", i, got)
		}
	}
	if got := admit(9, false); got != http.StatusConflict {
		t.Fatalf("tenth distinct entry = %d, want the breaker's conflict", got)
	}
	if got := admit(10, true); got != http.StatusAccepted {
		t.Fatalf("forced reacquire = %d, want accepted", got)
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
