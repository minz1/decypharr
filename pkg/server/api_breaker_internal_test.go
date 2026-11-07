package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
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
