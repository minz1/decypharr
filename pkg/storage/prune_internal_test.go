package storage

import (
	"strings"
	"testing"
	"time"
)

func openTestStorage(t *testing.T, dir string) *Storage {
	t.Helper()
	s, err := NewStorage(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUnreferencedLedgerRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	a := time.Unix(1_700_000_000, 123)
	b := time.Unix(1_700_005_000, 0)
	if err := s.SetUnreferencedSince("aaa", a); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUnreferencedSince("bbb", b); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearUnreferencedSince("aaa"); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnreferencedSince()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["bbb"].Unix() != b.Unix() {
		t.Fatalf("ledger = %v", got)
	}
	if err = s.SetUnreferencedSince("aaa", a); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UnreferencedSince()
	if got["aaa"].Unix() != a.Unix() || got["aaa"].Nanosecond() != 0 {
		t.Fatalf("want second precision, got %v", got["aaa"])
	}
}

func TestLedgerSurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openTestStorage(t, dir)
	if err := s.SetUnreferencedSince("aaa", time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastReferencedCount(12); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStorage(t, dir)
	defer s.Close()
	got, err := s.UnreferencedSince()
	if err != nil || got["aaa"].Unix() != 1_700_000_000 {
		t.Fatalf("ledger after reopen = %v, %v", got, err)
	}
	if n, ok, countErr := s.LastReferencedCount(); countErr != nil || !ok || n != 12 {
		t.Fatalf("count after reopen = %d %v %v", n, ok, countErr)
	}
}

func TestLastReferencedCount(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	if _, ok, err := s.LastReferencedCount(); err != nil || ok {
		t.Fatalf("unset: ok=%v err=%v", ok, err)
	}
	if err := s.SetLastReferencedCount(4357); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.LastReferencedCount(); err != nil || !ok || n != 4357 {
		t.Fatalf("set: %d %v %v", n, ok, err)
	}
	got, err := s.UnreferencedSince()
	if err != nil || len(got) != 0 {
		t.Fatalf("count leaked into ledger: %v %v", got, err)
	}
}

func TestUnreferencedSinceCorruptValue(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	if err := s.prune.Put("badhash", []byte{1, 2, 3}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnreferencedSince()
	if err == nil || !strings.Contains(err.Error(), "badhash") {
		t.Fatalf("want error naming the key, got %v", err)
	}
	if got != nil {
		t.Fatalf("want nil map, got %v", got)
	}
}

func TestLastScan(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	if _, ok, err := s.LastScan(); err != nil || ok {
		t.Fatalf("unset: ok=%v err=%v", ok, err)
	}
	when := time.Unix(1_800_000_000, 0)
	if err := s.SetLastScan(when); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.LastScan(); err != nil || !ok || !got.Equal(when) {
		t.Fatalf("set: %v %v %v", got, ok, err)
	}
	if got, err := s.UnreferencedSince(); err != nil || len(got) != 0 {
		t.Fatalf("scan time leaked into ledger: %v %v", got, err)
	}
}
