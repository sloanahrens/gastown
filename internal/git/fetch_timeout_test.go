package git

import (
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// A remote that accepts the connection and never answers must not hang the
// caller: the bounded fetch returns a timeout error.
func TestFetchRefspecWithTimeoutBoundsAHangingRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var (
		mu   sync.Mutex
		held []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // accept, then say nothing
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	g := NewGitWithDir(dir, "")
	start := time.Now()
	err = g.FetchRefspecWithTimeout("git://"+ln.Addr().String()+"/repo.git", "+refs/heads/main:refs/remotes/origin/main", time.Second)
	if err == nil {
		t.Fatal("want a timeout error from a hanging remote")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("bounded fetch took %v", elapsed)
	}
}
