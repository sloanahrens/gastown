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
		wantKilled bool
	}{
		{"a gt still running at the bound is killed", "exec sleep 60", true},
		{"a gt that fails on its own is not", "echo boom >&2\nexit 3", false},
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
			b := h.runRecoveryBatchWithin("gastown", 3, 300*time.Millisecond)
			if b.err == nil {
				t.Fatal("the batch answered without error")
			}
			if got := strings.Contains(b.err.Error(), "killed after"); got != tc.wantKilled {
				t.Fatalf("batch error %q: names a kill = %v, want %v", b.err, got, tc.wantKilled)
			}
			if tc.wantKilled {
				if !strings.Contains(b.err.Error(), "at the 300ms bound for 3 polecats") {
					t.Fatalf("killed batch error %q, want the bound and the seat count", b.err)
				}
				if took := time.Since(started); took > 30*time.Second {
					t.Fatalf("the deadline took %s to kill a 300ms-bound run", took)
				}
			}
		})
	}
}
