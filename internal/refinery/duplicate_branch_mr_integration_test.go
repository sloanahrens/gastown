package refinery

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// installDuplicateBranchMRsBDStub fakes `bd` with three open MRs in rig
// "gastown": gt-wisp-or5 and gt-wisp-1sab share branch polecat/jade/gt-fo3h
// (the gt-k1qf scenario), gt-wisp-ok is unrelated. All are unclaimed and
// unblocked, so ListReadyMRs' duplicate-branch exclusion is the only thing
// that can keep the colliding pair out of the ready list.
func installDuplicateBranchMRsBDStub(t *testing.T) {
	t.Helper()
	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	const sqlRows = `[` +
		`{"id":"gt-wisp-or5","title":"Merge: gt-fo3h","description":"branch: polecat/jade/gt-fo3h\ntarget: main\nsource_issue: gt-fo3h\nrig: gastown\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:merge-request"},` +
		`{"id":"gt-wisp-1sab","title":"Merge: gt-fo3h","description":"branch: polecat/jade/gt-fo3h\ntarget: main\nsource_issue: gt-fo3h\nrig: gastown\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:01:00Z","updated_at":"2026-06-29T00:01:00Z","created_by":"tester","labels_csv":"gt:merge-request"},` +
		`{"id":"gt-wisp-ok","title":"Merge: gt-1","description":"branch: polecat/nux/gt-1\ntarget: main\nsource_issue: gt-1\nrig: gastown\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:merge-request"}` +
		`]`
	const showRows = `[` +
		`{"id":"gt-wisp-or5","title":"Merge: gt-fo3h","description":"branch: polecat/jade/gt-fo3h\ntarget: main\nsource_issue: gt-fo3h\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","ephemeral":true,"labels":["gt:merge-request"]},` +
		`{"id":"gt-wisp-1sab","title":"Merge: gt-fo3h","description":"branch: polecat/jade/gt-fo3h\ntarget: main\nsource_issue: gt-fo3h\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:01:00Z","updated_at":"2026-06-29T00:01:00Z","ephemeral":true,"labels":["gt:merge-request"]},` +
		`{"id":"gt-wisp-ok","title":"Merge: gt-1","description":"branch: polecat/nux/gt-1\ntarget: main\nsource_issue: gt-1\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","ephemeral":true,"labels":["gt:merge-request"]}` +
		`]`

	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"${1:-}\" = \"--allow-stale\" ]; then\n" +
		"  if [ \"${2:-}\" = \"version\" ]; then\n" +
		"    echo \"Error: unknown flag: --allow-stale\" >&2\n" +
		"    exit 0\n" +
		"  fi\n" +
		"  shift\n" +
		"fi\n" +
		"case \"${1:-}\" in\n" +
		"  list)\n" +
		"    printf '%s\\n' '[]'\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  sql)\n" +
		"    printf '%s\\n' '" + sqlRows + "'\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  show)\n" +
		"    printf '%s\\n' '" + showRows + "'\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"  *)\n" +
		"    printf '%s\\n' '[]'\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n"

	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestListReadyMRs_ExcludesDuplicateBranchMRs wires the gt-k1qf fix end to
// end through Engineer.ListReadyMRs: two open MRs sharing a branch must
// never come back as ready, but an unrelated MR in the same rig must not be
// blocked by their presence (the 0.55 finding on the first attempt).
func TestListReadyMRs_ExcludesDuplicateBranchMRs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	installDuplicateBranchMRsBDStub(t)

	e := NewEngineer(&rig.Rig{Name: "gastown", Path: t.TempDir()})

	ready, err := e.ListReadyMRs()
	if err != nil {
		t.Fatalf("ListReadyMRs() error = %v", err)
	}
	var readyIDs []string
	for _, mr := range ready {
		readyIDs = append(readyIDs, mr.ID)
	}
	if got := strings.Join(readyIDs, ","); got != "gt-wisp-ok" {
		t.Fatalf("ready IDs = %q, want only gt-wisp-ok (gt-wisp-or5/gt-wisp-1sab share a branch)", got)
	}

	anomalies, err := e.ListQueueAnomalies(time.Now())
	if err != nil {
		t.Fatalf("ListQueueAnomalies() error = %v", err)
	}
	var dupIDs []string
	for _, a := range anomalies {
		if a.Type == "duplicate-branch" {
			dupIDs = append(dupIDs, a.ID)
		}
	}
	sort.Strings(dupIDs)
	if got := strings.Join(dupIDs, ","); got != "gt-wisp-1sab,gt-wisp-or5" {
		t.Fatalf("duplicate-branch anomaly IDs = %q, want gt-wisp-1sab,gt-wisp-or5 (excluded MRs must still be escalated)", got)
	}
}
