package reacquire

import "testing"

func TestServiceReadyOnlyWhileStarted(t *testing.T) {
	t.Parallel()
	service, err := NewService(ServiceOptions{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if service.Ready() {
		t.Fatal("ready before Start")
	}
	if err = service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !service.Ready() {
		t.Fatal("not ready after Start")
	}
	if err = service.Close(); err != nil {
		t.Fatal(err)
	}
	if service.Ready() {
		t.Fatal("ready after Close")
	}
}
