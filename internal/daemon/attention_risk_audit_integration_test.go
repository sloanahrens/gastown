//go:build integration

package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// writeRiskAuditBDStub puts a bd on disk that answers the calls the resolved-
// FAIL pass makes — `show` out of fixtures, anything else empty — and appends
// the argv of every call to logPath, so a test can check what the pass asked
// for. The daemon runs bd from d.bdPath, so the routing environment, the
// binary and the note text under test are what reaches this script
// (rig_status_integration_test.go is the same guard for showRigBead).
func writeRiskAuditBDStub(t *testing.T, binDir, logPath, fixtures string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--allow-stale" ]; then shift; fi
printf '%%s\n' "$*" >> %s
if [ "$1" = "version" ]; then
  echo "bd 9.9.9"
  exit 0
fi
if [ "$1" = "show" ]; then
  # bd show prints the issues of the ids it is given, the id in any position
  # next to --json: one fixture per id, each the bead's object, wrapped in one
  # array. The file is copied whole, so its layout does not matter.
  shift
  found=""
  for arg in "$@"; do
    case "$arg" in -*) continue ;; esac
    if [ -f %s/"$arg".json ]; then found="$found $arg"; fi
  done
  if [ -n "$found" ]; then
    first=1
    printf '['
    for id in $found; do
      [ "$first" = 1 ] || printf ','
      first=0
      cat %s/"$id".json
    done
    printf ']\n'
    exit 0
  fi
fi
echo '[]'
`, logPath, fixtures, fixtures)

	path := filepath.Join(binDir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing bd stub: %v", err)
	}
	return path
}

// writeStubBead answers `bd show <id> --json` for one bead: the fixture is the
// bead's object, and the stub wraps it in the array bd prints.
func writeStubBead(t *testing.T, fixtures, id string, fields map[string]any) {
	t.Helper()
	fields["id"] = id
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubCalls are the argv lines the bd stub recorded, with the --allow-stale the
// beads client prepends already stripped.
func stubCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the bd stub's log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestIntegrationRiskAuditBDStubShowsMultiLineFixture guards the stub's show
// path against a fixture that spans lines (om finding 923498dfe154): bd prints
// a JSON array of the beads it was given, whatever the layout of each file.
func TestIntegrationRiskAuditBDStubShowsMultiLineFixture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bd stub is a POSIX shell script")
	}
	fixtures := t.TempDir()
	const multiLine = "{\n  \"id\": \"tr-abc\",\n  \"labels\": [\n    \"om-followup\"\n  ]\n}\n"
	if err := os.WriteFile(filepath.Join(fixtures, "tr-abc.json"), []byte(multiLine), 0o644); err != nil {
		t.Fatal(err)
	}
	bdPath := writeRiskAuditBDStub(t, t.TempDir(), filepath.Join(t.TempDir(), "bd-calls"), fixtures)

	out, err := exec.Command(bdPath, "show", "tr-abc", "--json").Output()
	if err != nil {
		t.Fatalf("bd show: %v", err)
	}
	var beads []map[string]any
	if err := json.Unmarshal(out, &beads); err != nil {
		t.Fatalf("bd show did not print a JSON array: %v", err)
	}
	if len(beads) != 1 || beads[0]["id"] != "tr-abc" {
		t.Fatalf("bd show printed %d beads, want the one bead tr-abc", len(beads))
	}
}

// TestIntegrationAttentionRiskAuditWritesTheAuditedLine is the wiring guard for
// productionRiskAudit and the writeAttention call site (om finding 49a1da81f2b2):
// the read of a finding bead's status and close reason, and the append of the
// AUDITED line on the landed bead, run through the real bd client of a real
// heartbeat tick. Its unit twin, attention_risk_audit_test.go, answers both
// seams from a fake and proves the pass's own rules.
//
// The stub answers reads only, so the tick's own collector still sees the FAIL
// it just audited: what the queue holds says nothing here, and the line bd was
// asked to append is the whole of the evidence.
func TestIntegrationAttentionRiskAuditWritesTheAuditedLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bd stub is a POSIX shell script")
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	townRoot := t.TempDir()
	const rig = "testrig"
	registerTestRigs(t, townRoot, map[string]string{rig: "tr"})
	if err := os.MkdirAll(filepath.Join(townRoot, rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	lf, err := land.RigLandingsFile(townRoot, rig)
	if err != nil {
		t.Fatal(err)
	}
	if err := lf.Append(land.LandingRecord{BeadID: "tr-abc", Rig: rig, Head: head, Target: "main",
		LandedAt: now.Add(-time.Minute), RiskPaths: []string{"internal/daemon/attention.go"}}); err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	writeStubBead(t, fixtures, "tr-abc", map[string]any{"status": "open", "priority": 1, "notes": failNote(head, "tr-fa")})
	writeStubBead(t, fixtures, "tr-fa", map[string]any{"status": "closed", "close_reason": "fixed by tr-xyz"})
	logPath := filepath.Join(t.TempDir(), "bd-calls")

	logs := &bytes.Buffer{}
	d := &Daemon{
		config:     &Config{TownRoot: townRoot},
		logger:     log.New(logs, "", 0),
		clock:      clockwork.NewFakeClockAt(now),
		bdPath:     writeRiskAuditBDStub(t, t.TempDir(), logPath, fixtures),
		hostProbes: healthyHost(),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			LandingWorker: &LandingWorkerConfig{Enabled: true},
		}},
	}

	d.writeAttention()

	calls := strings.Join(stubCalls(t, logPath), "\n")
	if !strings.Contains(calls, "show tr-fa --json") {
		t.Errorf("bd calls = %q, want the finding bead read through the routing env", calls)
	}
	want := land.OverseerReviewMarker + " " + head + " AUDITED: findings resolved by tr-fa"
	if !strings.Contains(calls, "update tr-abc --append-notes "+want) {
		t.Errorf("bd calls = %q, want the AUDITED line appended to tr-abc", calls)
	}
	if !strings.Contains(logs.String(), "attention: audited resolved risk-path FAIL tr-abc: "+want) {
		t.Errorf("daemon log = %q, want the call site to log the line it wrote", logs.String())
	}
	if _, ok := attention.Find(mustReadState(t, townRoot), "risk:tr-abc:"+sha12(head)); !ok {
		t.Error("want the risk-path item in the queue the same tick wrote")
	}
}

// mustReadState is the queue the tick wrote, for a test that asserts on it.
func mustReadState(t *testing.T, townRoot string) attention.State {
	t.Helper()
	st, err := attention.ReadState(townRoot)
	if err != nil {
		t.Fatalf("reading the attention state: %v", err)
	}
	return st
}
