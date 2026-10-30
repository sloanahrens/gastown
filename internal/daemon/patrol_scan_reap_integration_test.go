//go:build integration && !windows

package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reap pass's recovery batch under a deadline that really kills: the gt
// here is a real process that outlives a short bound, so what the daemon
// reports is what exec reports for a command the deadline cut off, not a fake's
// canned exit status (gt-uscys).
func TestIntegrationReapRecoveryBatchDeadlineKillsARunningGT(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		script     string
		bound      time.Duration
		wantKilled bool
	}{
		// The bound belongs to the case, not to the test. A kill case wants it
		// short so the case costs 300ms. The case that fails on its own wants
		// no short bound at all: its property is that the error does not name a
		// kill, so the deadline must never be what ends that process. At the
		// kill case's scale a host that is merely busy can start a fresh script
		// after the deadline, and the sweep failed this case for exactly that
		// (gt-fbf5n) — the host's scheduler is not what this test judges.
		{"a gt still running at the bound is killed", "exec sleep 60", 300 * time.Millisecond, true},
		{"a gt that fails on its own is not", "echo boom >&2\nexit 3", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := reapHost(t, enabledCleanup("gastown"), nil, nil)
			h.d.logger = log.New(io.Discard, "", 0)
			h.d.gtPath = filepath.Join(t.TempDir(), "gt")
			if err := os.WriteFile(h.d.gtPath, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			started := time.Now()
			b := h.runRecoveryBatchWithin("gastown", 3, tc.bound)
			if b.err == nil {
				t.Fatal("the batch answered without error")
			}
			if got := strings.Contains(b.err.Error(), "killed after"); got != tc.wantKilled {
				t.Fatalf("batch error %q: names a kill = %v, want %v", b.err, got, tc.wantKilled)
			}
			if tc.wantKilled {
				if want := "at the " + tc.bound.String() + " bound for 3 polecats"; !strings.Contains(b.err.Error(), want) {
					t.Fatalf("killed batch error %q, want the bound and the seat count (%q)", b.err, want)
				}
				if took := time.Since(started); took > 30*time.Second {
					t.Fatalf("the deadline took %s to kill a %s-bound run", took, tc.bound)
				}
			}
		})
	}
}
