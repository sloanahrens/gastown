package deacon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// oneStrandedOps is a scan that finds one stranded convoy with a ready
// issue, so feedStranded reaches its dispatch step without a town.
func oneStrandedOps() feedOps {
	return feedOps{
		findStranded: func(string) ([]StrandedConvoy, error) {
			return []StrandedConvoy{{ID: "hq-cv-s1", Title: "s", TrackedCount: 1, ReadyCount: 1, ReadyIssues: []string{"gt-a"}}}, nil
		},
		closeEmpty: func(string, string) error { return nil },
	}
}

// stubStrandedGT puts a `gt` on PATH that logs every invocation, so a test
// sees whether the feed dog was slung.
func stubStrandedGT(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	bin := t.TempDir()
	logPath = filepath.Join(bin, "gt.log")
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "gt"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestFeedStranded_OperatorHold_DispatchesNoFeedDog: the feed dog slings the
// convoy's issues, so dispatching it during a hold is dispatching the work
// (gt-ifijm). The convoy is reported held, not fed, and no cooldown starts.
func TestFeedStranded_OperatorHold_DispatchesNoFeedDog(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	gtLog := stubStrandedGT(t)
	townRoot := holdTown(t)

	result := feedStranded(townRoot, 0, 0, oneStrandedOps())

	data, _ := os.ReadFile(gtLog)
	if strings.Contains(string(data), "sling") {
		t.Errorf("feed dog slung during an operator hold: %s", data)
	}
	if result.Fed != 0 {
		t.Errorf("Fed = %d, want 0", result.Fed)
	}
	found := false
	for _, d := range result.Details {
		if d.ConvoyID == "hq-cv-s1" && d.Action == "held" && strings.Contains(d.Message, "seat-refill.hold") {
			found = true
		}
	}
	if !found {
		t.Errorf("no held detail naming the hold; details: %+v", result.Details)
	}
	state, err := LoadFeedStrandedState(townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if cs, ok := state.Convoys["hq-cv-s1"]; ok && !cs.LastFeedTime.IsZero() {
		t.Errorf("a held convoy started its feed cooldown: %+v", cs)
	}
}

func TestFeedStranded_NoHold_DispatchesFeedDog(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	gtLog := stubStrandedGT(t)
	result := feedStranded(t.TempDir(), 0, 0, oneStrandedOps())
	data, _ := os.ReadFile(gtLog)
	if result.Fed != 1 || !strings.Contains(string(data), "sling mol-convoy-feed deacon/dogs") {
		t.Errorf("Fed = %d, gt calls %q; want one feed dog", result.Fed, data)
	}
}

// A rig ESTOP on the bead's target rig defers the re-sling the same way the
// town hold does.
func TestRedispatchRecoveredBead_RigEstopDefers(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	calls := stubDispatchTools(t)
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	result := RedispatchRecoveredBead(notifyfake.New(), RecoveredBeadRecord{}, townRoot, "gt-righeld", "gastown", 0, 0)

	if result.Action != "deferred" || !strings.Contains(result.Message, "ESTOP.gastown") {
		t.Fatalf("Action = %q, Message = %q; want deferred naming ESTOP.gastown", result.Action, result.Message)
	}
	if call := callInvoking(calls(), "gt", "sling "); call != "" {
		t.Errorf("re-slung into a rig under ESTOP: %s", call)
	}
}
