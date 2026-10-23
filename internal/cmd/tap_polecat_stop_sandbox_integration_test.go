//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// TestIntegrationScheduleSandboxSessionKill runs the real detached shell against
// a fake tmux on PATH. The shell passes the session name through as one argument
// even when it holds shell metacharacters, kills it in the sandbox's tmux
// server, and runs outside the hook's process group so the hook's exit does not
// take it down. The fake reports through a FIFO the test reads, so nothing waits
// on a timer.
func TestIntegrationScheduleSandboxSessionKill(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "report")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\n{ printf '%s\\n' \"$@\"; ps -o pgid= -p $$; } > '" + fifo + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	type result struct {
		out []byte
		err error
	}
	got := make(chan result, 1)
	received := false
	go func() {
		out, err := os.ReadFile(fifo) // returns at EOF, when the fake tmux exits
		got <- result{out, err}
	}()
	t.Cleanup(func() {
		if received {
			return
		}
		// The fake never ran: open the FIFO for writing and close it so the read ends.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close()
		}
		<-got
	})

	const name = `gt-a b;$(echo pwned)'"`
	if err := scheduleSandboxSessionKill(name, 0); err != nil {
		t.Fatalf("scheduleSandboxSessionKill: %v", err)
	}
	var r result
	select {
	case r = <-got:
		received = true
	case <-time.After(30 * time.Second):
		t.Fatal("the fake tmux was never run")
	}
	if r.err != nil {
		t.Fatalf("reading the fake tmux report: %v", r.err)
	}
	lines := strings.Split(strings.TrimRight(string(r.out), "\n"), "\n")
	want := []string{"-L", session.SandboxTmuxSocket, "kill-session", "-t", name}
	if len(lines) != len(want)+1 || strings.Join(lines[:len(want)], "\n") != strings.Join(want, "\n") {
		t.Fatalf("tmux ran with %q, want %q then a process group", lines, want)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(lines[len(want)]))
	if err != nil {
		t.Fatalf("process group %q: %v", lines[len(want)], err)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("the kill runs in the hook's process group %d, so the hook's exit can take it down", pgid)
	}
}
