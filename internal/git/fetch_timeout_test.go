package git

import (
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallingHTTPRemote serves an http:// URL whose server accepts every
// connection and never answers. Over http git forks git-remote-http, which
// inherits git's output pipes; killing only git leaves the helper holding
// them and Run() blocked. Returns the URL and a function yielding the
// connections accepted so far.
func stallingHTTPRemote(t *testing.T) (string, func() []net.Conn) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Keep a proxy from the environment out of the path to the listener.
	for _, k := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
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
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return "http://" + ln.Addr().String() + "/repo.git", func() []net.Conn {
		mu.Lock()
		defer mu.Unlock()
		return append([]net.Conn(nil), held...)
	}
}

// assertBoundedAndHelperGone checks that a timed remote call returned a
// timeout within its bound plus the process-group kill grace, and that the
// remote helper holding the connection is dead (the server sees EOF).
func assertBoundedAndHelperGone(t *testing.T, err error, elapsed, bound time.Duration, conns []net.Conn) {
	t.Helper()
	if err == nil {
		t.Fatal("want a timeout error from a stalling remote")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout, got %v", err)
	}
	// bound + SIGTERM grace + scheduling slack; a surviving helper holding
	// the pipe blocks for as long as the server stalls (unbounded).
	if limit := bound + 6*time.Second; elapsed > limit {
		t.Fatalf("bounded call took %v (limit %v)", elapsed, limit)
	}
	t.Logf("returned in %v (bound %v)", elapsed.Round(10*time.Millisecond), bound)
	if len(conns) == 0 {
		t.Fatal("the remote helper never connected; the test did not exercise the http path")
	}
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 4096)
		for {
			_, rerr := c.Read(buf)
			if rerr == nil {
				continue // drain the request the helper sent
			}
			var ne net.Error
			if errors.As(rerr, &ne) && ne.Timeout() {
				t.Fatal("remote helper still holds its connection after the call returned")
			}
			if !errors.Is(rerr, io.EOF) && !strings.Contains(rerr.Error(), "reset") {
				t.Fatalf("reading helper connection: %v", rerr)
			}
			break
		}
	}
}

// A fetch from an http remote that stalls must return near its bound: the
// whole process group (git and git-remote-http) is killed.
func TestFetchRefspecWithTimeoutBoundsAStallingHTTPRemote(t *testing.T) {
	url, conns := stallingHTTPRemote(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	g := NewGitWithDir(dir, "")
	const bound = time.Second
	start := time.Now()
	err := g.FetchRefspecWithTimeout(url, "+refs/heads/main:refs/remotes/origin/main", bound)
	assertBoundedAndHelperGone(t, err, time.Since(start), bound, conns())
}

// ls-remote against the same stalling http remote is bounded the same way.
func TestListRemoteRefsWithHashesTimeoutBoundsAStallingHTTPRemote(t *testing.T) {
	url, conns := stallingHTTPRemote(t)
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	g := NewGitWithDir(dir, "")
	const bound = time.Second
	start := time.Now()
	_, err := g.ListRemoteRefsWithHashesTimeout(url, "refs/heads/polecat/", bound)
	assertBoundedAndHelperGone(t, err, time.Since(start), bound, conns())
}
