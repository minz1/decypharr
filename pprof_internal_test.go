package main

import (
	"context"
	"net"
	"testing"
)

func TestServePprofStopsOnCancel(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	result := make(chan error, 1)
	go func() { result <- servePprofOn(ctx, ln) }()

	cancel()
	if got := <-result; got != nil {
		t.Fatalf("servePprofOn after cancel = %v, want nil", got)
	}

	conn, err := net.Dial("tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("listener %s still accepts connections after servePprofOn returned", addr)
	}
}
