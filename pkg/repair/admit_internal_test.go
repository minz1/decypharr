package repair

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func newAdmitService(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	service := New(Dependencies{Storage: newRepairTestStorage(t)})
	clock := newFakeClock()
	service.now = clock.Now
	return service, clock
}

func TestAdmitTripsAtTenDistinctEntriesAndReturnsAHeldError(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	req := func(i int) HealRequest {
		return HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("entry-%d", i)}
	}
	for i := range 9 {
		if err := service.Admit(t.Context(), req(i)); err != nil {
			t.Fatalf("entry %d refused before the threshold: %v", i, err)
		}
	}
	err := service.Admit(t.Context(), req(9))
	var held *HeldError
	if !errors.Is(err, ErrHealHeld) || !errors.As(err, &held) || held.Bucket != rdHoster() {
		t.Fatalf("tenth entry = %v, want a held error for rd/hoster_link", err)
	}
	// Another class of the same provider is a different bucket.
	if err = service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassDropped, Subject: "x"}); err != nil {
		t.Fatalf("a different class was held: %v", err)
	}
}

func TestAdmitReleasesATrippedBucketAfterTheHoldWithAPassingCanary(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome { return canaryPass }
	for i := range 10 {
		_ = service.Admit(
			t.Context(),
			HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("e%d", i)},
		)
	}
	one := HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "late"}
	if err := service.Admit(t.Context(), one); !errors.Is(err, ErrHealHeld) {
		t.Fatalf("a held bucket admitted: %v", err)
	}
	clock.Advance(61 * time.Minute) // the window and the hold have both run out
	if err := service.Admit(t.Context(), one); err != nil {
		t.Fatalf("admit after the hold = %v, want released by the passing canary", err)
	}
	if len(service.BreakerStatus()) != 0 {
		t.Fatalf("bucket still open: %+v", service.BreakerStatus())
	}
}

func TestAdmitForceBypassesAndReleasesTheBreakerAndCounts(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	for i := range 12 {
		_ = service.Admit(
			t.Context(),
			HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("e%d", i)},
		)
	}
	forced := HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "forced", Force: true, Caller: "ops"}
	if err := service.Admit(t.Context(), forced); err != nil {
		t.Fatalf("forced admit = %v", err)
	}
	if len(service.BreakerStatus()) != 0 {
		t.Fatalf("a forced heal is the operator's evidence and releases the bucket: %+v", service.BreakerStatus())
	}
}

func TestAdmitCapsHealsPerBucket(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	for i := range 25 {
		if err := service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassOther}); err != nil {
			t.Fatalf("heal %d refused: %v", i, err)
		}
	}
	err := service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassOther})
	if !errors.Is(err, ErrHealCapped) {
		t.Fatalf("26th heal = %v, want the cap error", err)
	}
}

func TestAdmitCapsHealsAcrossBuckets(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	admitted := 0
	for _, provider := range []string{"a", "b", "c"} {
		for range 25 {
			if service.Admit(t.Context(), HealRequest{Provider: provider, Class: ClassOther}) == nil {
				admitted++
			}
		}
	}
	if admitted != 50 {
		t.Fatalf("admitted %d heals in the window, want the 50 total cap", admitted)
	}
}

// The compensating search of a reacquisition deletes nothing and must always be able to finish a heal that was
// already admitted, so the reacquire package cannot reach the breaker at all.
func TestReacquirePackageCannotReachTheBreaker(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "arr", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob(filepath.Join("..", "arr", "reacquire", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, file := range append(files, more...) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		checked++
		for _, spec := range parsed.Imports {
			if strings.HasSuffix(strings.Trim(spec.Path.Value, `"`), "/pkg/repair") {
				t.Errorf("%s imports pkg/repair; the compensating search could then be put through the breaker", file)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no arr source files found")
	}
}

// Every Arr-file-deleting heal in the repair package is decided by healBatch; healBrokenEntry is only its executor.
func TestHealBrokenEntryIsOnlyCalledFromTheBatch(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "healBrokenEntry" {
				callers = append(callers, file)
			}
			return true
		})
	}
	if want := []string{"batch.go"}; !slices.Equal(callers, want) {
		t.Fatalf("healBrokenEntry is called from %v, want only %v", callers, want)
	}
}

// previousGate is the confirm gate of the binary that predates the breaker, kept verbatim: it confirms only a
// file stored on a row whose status was exactly broken.
func previousGate(previous storage.HealthStatus, stored, broken []storage.BrokenFile) []storage.BrokenFile {
	if previous != storage.HealthBroken {
		return nil
	}
	key := func(file storage.BrokenFile) string { return file.InfoHash + "\x00" + file.FileName }
	confirmable := make(map[string]bool, len(stored))
	for _, file := range stored {
		confirmable[key(file)] = true
	}
	return slices.DeleteFunc(slices.Clone(broken), func(file storage.BrokenFile) bool {
		return !confirmable[key(file)]
	})
}

func TestAHeldRowCannotBeConfirmedByThePreviousBinary(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)

	row := f.health(t, 0)
	if row.Status != storage.HealthHeld || len(row.BrokenFiles) == 0 {
		t.Fatalf("row = %#v, want a held row that still names its files", row)
	}
	if got := previousGate(row.Status, row.BrokenFiles, row.BrokenFiles); len(got) != 0 {
		t.Fatalf("the previous gate confirms a held row: %v", got)
	}
	if !row.IsDue(f.clock.Now(), 7*24*time.Hour) {
		t.Fatal("the previous IsDue would skip a held row; it must be re-probed")
	}
	// The previous collectBrokenHealths and FixBroken select Status == broken only.
	if row.Status == storage.HealthBroken {
		t.Fatal("a held row is selectable as broken")
	}
}
