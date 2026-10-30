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

	"github.com/jonboulle/clockwork"
)

// The reap pass's recovery batch under a deadline that really kills: the gt
// here is a real process that outlives the bound, so what the daemon reports
// is what exec reports for a command the deadline cut off, not a fake's canned
// exit status (gt-uscys).
//
// The deadline runs on the daemon's clock, so each case says whether it reaches
// the bound: the killed case advances the fake clock once the script is
// running, and the case that fails on its own never does, leaving the script's
// exit status to end the run (gt-fbf5n).
func TestIntegrationReapRecoveryBatchDeadlineKillsARunningGT(t *testing.T) {
	t.Parallel()
	// The bound is fake time, so it costs the cases nothing; what they spend is
	// the scripts' own running time.
	const bound = 300 * time.Millisecond
	for _, tc := range []struct {
		name       string
		script     string
		toBound    bool
		wantKilled bool
	}{
		{"a gt still running at the bound is killed", "touch {marker}\nexec sleep 3", true, true},
		// The script outruns the bound in real time, so a deadline read from
		// the wall clock would cut it off here; its own exit status has to be
		// what ends the run.
		{"a gt that fails on its own is not", "sleep 1\necho boom >&2\nexit 3", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := reapHost(t, enabledCleanup("gastown"), nil, nil)
			h.d.logger = log.New(io.Discard, "", 0)
			fc := clockwork.NewFakeClockAt(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
			h.d.clock = fc
			dir := t.TempDir()
			h.d.gtPath = filepath.Join(dir, "gt")
			marker := filepath.Join(dir, "started")
			script := strings.ReplaceAll(tc.script, "{marker}", marker)
			if err := os.WriteFile(h.d.gtPath, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			done := make(chan reapBatch, 1)
			go func() { done <- h.runRecoveryBatchWithin("gastown", 3, bound) }()
			if tc.toBound {
				// The marker is the script's own word that it is running, so the
				// advance kills a live process instead of landing on a start the
				// deadline reached first.
				if !waitFor(30*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
					t.Fatal("the script never said it was running")
				}
				fc.Advance(bound)
			}
			var b reapBatch
			select {
			case b = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the batch did not return within 30s of the bound")
			}
			if b.err == nil {
				t.Fatal("the batch answered without error")
			}
			if got := strings.Contains(b.err.Error(), "killed after"); got != tc.wantKilled {
				t.Fatalf("batch error %q: names a kill = %v, want %v", b.err, got, tc.wantKilled)
			}
			if tc.wantKilled {
				if want := "at the " + bound.String() + " bound for 3 polecats"; !strings.Contains(b.err.Error(), want) {
					t.Fatalf("killed batch error %q, want the bound and the seat count (%q)", b.err, want)
				}
				return
			}
			if want := "exit status 3"; !strings.Contains(b.err.Error(), want) {
				t.Fatalf("batch error %q, want the script's own failure (%q)", b.err, want)
			}
		})
	}
}
