//go:build integration && !windows

package tmux

import (
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func TestIntegrationNewSessionAllowsStaleUnixSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := uniqueSocketName(t, "gt-h9z-newsession-stale")
	createStaleUnixSocket(t, socket)
	tm := NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	if err := tm.NewSession("gt-h9z-stale-ok", ""); err != nil {
		t.Fatalf("NewSession against stale Unix socket = %v, want success", err)
	}
}

func TestIntegrationNewSessionRefusesUnresponsiveSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := uniqueSocketName(t, "gt-h9z-unresponsive")
	listener, socketPath := listenOnSocketPath(t, socket)
	var heldMu sync.Mutex
	var held []*net.UnixConn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, conn)
			heldMu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		heldMu.Lock()
		defer heldMu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})

	before, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", socketPath, err)
	}
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		errCh <- NewTmuxWithSocket(socket).NewSession("gt-h9z-unresponsive", "")
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("NewSession against unresponsive listener = nil, want error")
		}
		if elapsed := time.Since(start); elapsed > newSessionSocketProbeTimeout+2*time.Second {
			t.Fatalf("NewSession took %s, want bounded refusal", elapsed)
		}
	case <-time.After(newSessionSocketProbeTimeout + 3*time.Second):
		_ = listener.Close()
		t.Fatal("NewSession against unresponsive listener hung")
	}
	assertSameSocketPath(t, socketPath, before)
}

func TestIntegrationNewSessionRefusesClosingListener(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := uniqueSocketName(t, "gt-h9z-closing")
	listener, socketPath := listenOnSocketPath(t, socket)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})

	before, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", socketPath, err)
	}
	if err := NewTmuxWithSocket(socket).NewSession("gt-h9z-closing", ""); err == nil {
		t.Fatal("NewSession against closing listener = nil, want error")
	}
	assertSameSocketPath(t, socketPath, before)
}
