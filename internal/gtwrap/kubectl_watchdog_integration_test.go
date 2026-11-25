//go:build integration

package gtwrap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// integrationWatchdogLimit is the ceiling on one watchdog run. It is not a wait: each
// test returns as soon as its script exits, and a script still running at the
// ceiling is killed by its CommandContext and fails the test.
const integrationWatchdogLimit = 30 * time.Second

// TestIntegrationWatchdogKillsOnStdinEOF runs the production watchdog under the real
// sh with a child that never exits. The child prints its pid and then execs sleep,
// which keeps that pid, so the pid is the sleep's. Closing the script's stdin must
// make the watchdog kill the child: the script exits 143 and the child is gone.
func TestIntegrationWatchdogKillsOnStdinEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationWatchdogLimit)
	defer cancel()
	argv, err := watchdogArgv([]string{"sh", "-c", `echo $$; exec sleep 1000000`})
	if err != nil {
		t.Fatalf("watchdogArgv = %v", err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe = %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe = %v", err)
	}
	defer outR.Close()
	defer outW.Close()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdinR
	cmd.Stdout = outW
	ran := runCommand(cmd)
	pid, err := readChildPid(ctx, outR)
	if err != nil {
		t.Fatalf("the child did not print its pid: %v", err)
	}
	t.Cleanup(func() {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := stdinW.Close(); err != nil {
		t.Fatalf("close the script's stdin: %v", err)
	}
	var runErr error
	select {
	case runErr = <-ran:
	case <-ctx.Done():
		t.Fatalf("the script did not exit within %s after its stdin closed: %v", integrationWatchdogLimit, ctx.Err())
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		t.Fatalf("Run = %v", runErr)
	}
	if got := cmd.ProcessState.ExitCode(); got != 143 {
		t.Errorf("the script exited %d; want 143, the child's status after the watchdog's TERM", got)
	}
	if processAlive(pid) {
		t.Errorf("the child %d is still running after the script exited", pid)
	}
}

// TestIntegrationWatchdogPassesTheChildStatus runs the production watchdog under the
// real sh with a child that exits on its own. The script must exit with the child's
// status while its stdin is still open, so it does not wait for the caller to close it.
func TestIntegrationWatchdogPassesTheChildStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"a child that exits 0", 0},
		{"a child that exits 5", 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationWatchdogLimit)
			defer cancel()
			argv, err := watchdogArgv([]string{"sh", "-c", fmt.Sprintf("exit %d", tc.status)})
			if err != nil {
				t.Fatalf("watchdogArgv = %v", err)
			}
			// The pipe's write end stays open for the whole run: the script must finish
			// without its stdin ever closing.
			stdinR, stdinW, err := os.Pipe()
			if err != nil {
				t.Fatalf("stdin pipe = %v", err)
			}
			defer stdinR.Close()
			defer stdinW.Close()
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Stdin = stdinR
			ran := runCommand(cmd)
			var runErr error
			select {
			case runErr = <-ran:
			case <-ctx.Done():
				t.Fatalf("the script waited for its stdin: it did not exit within %s: %v", integrationWatchdogLimit, ctx.Err())
			}
			var exitErr *exec.ExitError
			if runErr != nil && !errors.As(runErr, &exitErr) {
				t.Fatalf("Run = %v", runErr)
			}
			if got := cmd.ProcessState.ExitCode(); got != tc.status {
				t.Errorf("the script exited %d; want %d, the child's own status", got, tc.status)
			}
		})
	}
}

// runCommand runs cmd on a goroutine and returns the channel its Run error arrives on.
// The caller bounds the receive with a select on its context, and CommandContext kills
// the command at that ceiling, so a command that never exits cannot hold the test.
func runCommand(cmd *exec.Cmd) <-chan error {
	ran := make(chan error, 1)
	go func() { ran <- cmd.Run() }()
	return ran
}

// readChildPid reads the first line the child prints and parses it as a pid. The read
// runs beside the context, so a child that never prints fails at the ceiling.
func readChildPid(ctx context.Context, r io.Reader) (int, error) {
	type reply struct {
		pid int
		err error
	}
	got := make(chan reply, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil {
			got <- reply{err: err}
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		got <- reply{pid: pid, err: err}
	}()
	select {
	case res := <-got:
		return res.pid, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// processAlive reports whether pid names a process that still exists. EPERM means it
// exists under another owner, so it counts as alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
