package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// testTmuxSocket is the socket this package's tmux commands are scoped to. The
// package cannot reach internal/tmux or internal/testutil for it: tmux imports
// internal/config, so either import is a cycle from a test in this package.
// Unscoped, the integration test's `tmux new-session` lands on the server the
// invoking agent shell runs inside and reads as a phantom polecat (gt-2bj).
var testTmuxSocket = fmt.Sprintf("gt-test-config-%d", os.Getpid())

// testTmuxCommand builds a tmux command scoped to testTmuxSocket: -L outranks
// the inherited $TMUX, so every tmux call in this package must go through it.
func testTmuxCommand(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", testTmuxSocket}, args...)...)
}

func TestMain(m *testing.M) {
	// Drop the invoking pane's tmux identity for the whole process, so a bare
	// `tmux` added to a test later cannot reach the live server either.
	_ = os.Unsetenv("TMUX")
	_ = os.Unsetenv("TMUX_PANE")

	stubDir, err := os.MkdirTemp("", "gt-agent-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create stub dir: %v\n", err)
		os.Exit(1)
	}

	binaries := []string{
		"claude",
		"gemini",
		"codex",
		"cursor-agent",
		"auggie",
		"amp",
		"opencode",
	}
	for _, name := range binaries {
		path := filepath.Join(stubDir, name)
		stub := []byte("#!/bin/sh\nexit 0\n")
		mode := os.FileMode(0755)
		if runtime.GOOS == "windows" {
			path += ".cmd"
			stub = []byte("@echo off\r\nexit /b 0\r\n")
			mode = 0644
		}
		if err := os.WriteFile(path, stub, mode); err != nil {
			fmt.Fprintf(os.Stderr, "write stub %s: %v\n", name, err)
			os.Exit(1)
		}
	}

	originalPath := os.Getenv("PATH")
	_ = os.Setenv("PATH", stubDir+string(os.PathListSeparator)+originalPath)
	// cursor_agent_cli_test.go skips this directory when resolving a real cursor-agent
	// (must stay in sync — do not rename without updating that resolver).
	_ = os.Setenv("GT_AGENT_STUB_BIN_DIR", stubDir)

	code := m.Run()

	// The isolated server outlives the test that started it: kill it whole, so
	// no socket file or stray server is left for the next run to trip over.
	if _, err := exec.LookPath("tmux"); err == nil {
		_ = exec.Command("tmux", "-L", testTmuxSocket, "kill-server").Run()
	}

	_ = os.Setenv("PATH", originalPath)
	_ = os.Unsetenv("GT_AGENT_STUB_BIN_DIR")
	_ = os.RemoveAll(stubDir)
	os.Exit(code)
}
