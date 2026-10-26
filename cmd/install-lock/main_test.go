package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	cases := map[string][]string{
		"no arguments":  {},
		"no lock path":  {"--wait", "1", "--", "true"},
		"negative wait": {"--lock", lock, "--wait", "-1", "--", "true"},
		"no command":    {"--lock", lock, "--wait", "1"},
		"unknown flag":  {"--lock", lock, "--nope", "--", "true"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr strings.Builder
			if got := run(args, &stderr); got != 2 {
				t.Errorf("run(%v) = %d, want 2 (stderr: %s)", args, got, stderr.String())
			}
		})
	}
}

// A held lock with no wait is the one path run can finish on without exec'ing:
// it returns the lock-busy status the script maps to its exit 3.
func TestHeldLockWithNoWaitExitsSeventyFive(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	var stderr strings.Builder

	got := run([]string{"--lock", lock, "--wait", "0", "--", "true"}, &stderr)

	if got != 75 {
		t.Errorf("run with the lock held = %d, want 75 (stderr: %s)", got, stderr.String())
	}
}
