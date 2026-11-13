package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func indexView(key string, phase grab.Phase) GrabView {
	return GrabView{
		State: grab.State{Key: key, Phase: phase},
		Entry: storage.Entry{InfoHash: key, Tags: []string{"t"}},
	}
}

func TestGrabIndexViewsAreCopies(t *testing.T) {
	t.Parallel()
	var idx grabIndex
	v := indexView("k", grab.PhaseProvider)
	idx.publish(v)
	v.Entry.Tags[0] = "changed"
	got, ok := idx.get("k")
	if !ok || got.Entry.Tags[0] != "t" {
		t.Fatalf("published view changed with its source: %v", got.Entry.Tags)
	}
	got.Entry.Tags[0] = "changed"
	again, _ := idx.get("k")
	if again.Entry.Tags[0] != "t" {
		t.Fatal("a reader's copy changed the index")
	}
}

func TestGrabIndexKeysAreCaseInsensitive(t *testing.T) {
	t.Parallel()
	var idx grabIndex
	idx.publish(indexView("ABC", grab.PhaseQueued))
	if _, ok := idx.get("abc"); !ok {
		t.Fatal("lower-cased key not found")
	}
	idx.remove("Abc")
	if _, ok := idx.get("ABC"); ok {
		t.Fatal("remove with another case left the view")
	}
}

func TestGrabIndexActive(t *testing.T) {
	t.Parallel()
	for _, p := range grab.AllPhases() {
		var idx grabIndex
		idx.publish(indexView("k", p))
		want := p != grab.PhaseComplete && p != grab.PhaseFailed && p != grab.PhaseAbsent
		if got := idx.active("k"); got != want {
			t.Errorf("active in %v = %v, want %v", p, got, want)
		}
	}
	var empty grabIndex
	if empty.active("k") {
		t.Fatal("a missing key is active")
	}
}

func TestGrabIndexSnapshotIsStable(t *testing.T) {
	t.Parallel()
	var idx grabIndex
	idx.publish(indexView("a", grab.PhaseQueued))
	snap := idx.snapshot()
	idx.publish(indexView("a", grab.PhaseActing))
	idx.publish(indexView("b", grab.PhaseQueued))
	if len(snap) != 1 || snap[0].State.Phase != grab.PhaseQueued {
		t.Fatalf("snapshot changed after publish: %+v", snap)
	}
	snap[0].Entry.Tags[0] = "changed"
	if got, _ := idx.get("a"); got.Entry.Tags[0] != "t" {
		t.Fatal("a snapshot shares the index's entry")
	}
}
