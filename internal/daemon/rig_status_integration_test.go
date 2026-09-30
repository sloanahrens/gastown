//go:build integration

package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// rigStatusBDStub is a POSIX shell stub for the bd the rig-status lookup shells
// out to: the real lookup reads the rig's identity bead with `bd show`, and the
// whole point of gt-4nu3 is what happens when that subprocess is slow or fails.
// Each `show` served is appended to countPath, so a test can prove the memoized
// path is not re-consulted.
//
// The capability probe (`bd --allow-stale version`) is answered without
// counting and without honoring showScript: it runs before every real call, and
// a probe that slept out showScript's timeout would make the daemon read "bd
// does not support --allow-stale" instead of exercising the branch under test.
func writeRigStatusBDStub(t *testing.T, binDir, countPath, showScript string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("rig-status stubs are POSIX shell scripts")
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "version" ] || [ "$2" = "version" ]; then
  echo "bd 9.9.9"
  exit 0
fi
if [ "$1" = "show" ] || [ "$2" = "show" ]; then
  echo show >> %s
fi
%s
`, countPath, showScript)

	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing bd stub: %v", err)
	}
	return path
}

// rigStatusStubMissing answers the way bd does when the identity bead is not
// there. TestIntegrationIsRigOperational_MissingBeadThroughBD runs it through
// the real bd read: the wiring guard for showRigBead's default.
const rigStatusStubMissing = `echo 'Error: no issue found: tr-rig-testrig' >&2
exit 1`

func newRigStatusFixture(t *testing.T, showScript string) *rigStatusFixture {
	t.Helper()

	townRoot := t.TempDir()
	const rigName = "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	// The prefix is what turns the rig name into its identity bead ID
	// (tr-rig-testrig), which is the read this bead is about.
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"beads":{"prefix":"tr"}}`), 0o644); err != nil {
		t.Fatalf("writing rig config.json: %v", err)
	}

	binDir := t.TempDir()
	countPath := filepath.Join(t.TempDir(), "bd-shows")
	writeRigStatusBDStub(t, binDir, countPath, showScript)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logBuf := &bytes.Buffer{}
	alerts := &rigStatusAlerts{}
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(logBuf, "", 0),
	}
	// Stand in for the daemon's escalateAlert/clearAlerts, which shell out to
	// `gt escalate`: the escalation must be observable, not a real town write.
	d.rigStatusAlert = alerts.alert
	d.rigStatusClear = alerts.clear

	return &rigStatusFixture{
		daemon:    d,
		townRoot:  townRoot,
		rigName:   rigName,
		binDir:    binDir,
		countPath: countPath,
		logBuf:    logBuf,
		alerts:    alerts,
	}
}

// TestIntegrationIsRigOperational_MissingBeadThroughBD is the wiring guard
// for showRigBead's default: the real beads read of a bd that reports no such
// issue must reach the missing-bead branch. The unit twin,
// TestIsRigOperational_MissingBeadIsDistinctFromTimeout, covers the second half
// of criterion four of gt-4nu3: a missing identity bead and a starved-host
// timeout used to produce the same "(assuming not operational)" line, and they
// call for different responses (data repair vs. a saturated host).
func TestIntegrationIsRigOperational_MissingBeadThroughBD(t *testing.T) {
	f := newRigStatusFixture(t, rigStatusStubMissing)

	operational, reason := f.daemon.isRigOperational(f.rigName)
	if operational {
		t.Fatal("a missing identity bead must fail closed")
	}
	if !strings.Contains(reason, "cannot verify rig status") || !strings.Contains(reason, "rig bead missing") {
		t.Errorf("reason = %q, want it to name the missing bead", reason)
	}

	logged := f.logBuf.String()
	if !strings.Contains(logged, "rig bead missing") {
		t.Errorf("log must name the missing bead, got:\n%s", logged)
	}
	if strings.Contains(logged, "lookup timed out") {
		t.Errorf("a missing bead must not be logged as a timeout, got:\n%s", logged)
	}

	waitForRigStatusAlert(t, f, "the missing-bead escalation", func() bool {
		raised, _, _ := f.alerts.snapshot()
		return len(raised) == 1
	})
	_, messages, _ := f.alerts.snapshot()
	if !strings.Contains(messages[0], "rig bead missing") {
		t.Errorf("escalation message = %q, want it to name the missing bead", messages[0])
	}
}
