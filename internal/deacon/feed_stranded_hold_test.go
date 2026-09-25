package deacon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubStrandedGT puts a `gt` on PATH that reports one stranded convoy with a
// ready issue and logs every invocation, so FeedStranded reaches its
// dispatch step without a town.
func stubStrandedGT(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	bin := t.TempDir()
	logPath = filepath.Join(bin, "gt.log")
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '[{"id":"hq-cv-s1","title":"s","tracked_count":1,"ready_count":1,"ready_issues":["gt-a"]}]'
fi
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

	result := FeedStranded(townRoot, 0, 0)

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
	result := FeedStranded(t.TempDir(), 0, 0)
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

	result := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-righeld", "gastown", 0, 0)

	if result.Action != "deferred" || !strings.Contains(result.Message, "ESTOP.gastown") {
		t.Fatalf("Action = %q, Message = %q; want deferred naming ESTOP.gastown", result.Action, result.Message)
	}
	if call := callInvoking(calls(), "gt", "sling "); call != "" {
		t.Errorf("re-slung into a rig under ESTOP: %s", call)
	}
}
