package storage

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestOldShapedRecordsLoadAndNewViolationsAreCounted(t *testing.T) {
	t.Parallel()
	s, err := NewStorage(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	data, err := proto.Marshal(&EntryProto{InfoHash: "old", ActiveProvider: "gone", IsComplete: true,
		Providers: map[string]*ProviderEntryProto{"rd": {Provider: "rd", Status: "downloaded"}}})
	if err != nil {
		t.Fatal(err)
	}
	if putErr := s.entries.Put("old", data, nil); putErr != nil {
		t.Fatal(putErr)
	}
	got, err := s.Get("old")
	if err != nil || got.ActiveProvider != "gone" {
		t.Fatalf("old-shaped record must load as stored: %v %+v", err, got)
	}
	if n := s.InvariantViolations(); n != 0 {
		t.Fatalf("a load counted %d violations", n)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a violating write must fail in tests")
			}
		}()
		_ = s.AddOrUpdate(&Entry{InfoHash: "bad", ActiveProvider: "gone"})
	}()
	if n := s.InvariantViolations(); n != 1 {
		t.Fatalf("violations=%d want 1", n)
	}
}
