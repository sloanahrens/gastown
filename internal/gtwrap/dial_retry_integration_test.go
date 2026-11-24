//go:build integration

package gtwrap

import (
	"net"
	"testing"
	"time"
)

// TestIntegrationDialRetryRealClosedPort dials a loopback port held by no
// listener and checks the operating system's refusal is one RetryableDial
// accepts. The unit tier scripts this shape instead: a real dial carries a
// wall-clock deadline and blocks in a socket syscall, which no-network forbids
// there.
func TestIntegrationDialRetryRealClosedPort(t *testing.T) {
	// Port 1 on loopback is never an ephemeral port and nothing listens on it,
	// so a dial reaches it and is refused. Binding then releasing an ephemeral
	// port instead would race another process for the freed port.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:1", 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("dial 127.0.0.1:1 succeeded, want a refusal from a port with no listener")
	}
	if !RetryableDial(err) {
		t.Fatalf("RetryableDial(dial 127.0.0.1:1: %v) = false, want true for a refused dial", err)
	}
}
