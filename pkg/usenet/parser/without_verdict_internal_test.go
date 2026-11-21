package parser

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestWithoutVerdictZeroesTheOutcomeAndSharesTheRest(t *testing.T) {
	t.Parallel()
	groups := map[string]*FileGroup{"a": {}}
	parsed := &Parsed{
		NZB:     &storage.NZB{ID: "x"},
		Groups:  groups,
		Outcome: Outcome{Verdict: VerdictNone, Rule: outcomeRuleErrorChain},
	}
	kept := parsed.WithoutVerdict()
	if kept.Outcome != (Outcome{}) {
		t.Fatalf("Outcome = %+v, want zero", kept.Outcome)
	}
	if kept == parsed || kept.NZB != parsed.NZB || kept.Groups["a"] != groups["a"] {
		t.Fatal("WithoutVerdict must copy the Parsed and share its NZB and Groups")
	}
	if parsed.Outcome == (Outcome{}) {
		t.Fatal("the original Parsed lost its outcome")
	}
}
