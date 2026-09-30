//go:build !windows

package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEtime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected int
		wantErr  bool
	}{
		// MM:SS format
		{"00:30", 30, false},
		{"01:00", 60, false},
		{"01:23", 83, false},
		{"59:59", 3599, false},

		// HH:MM:SS format
		{"00:01:00", 60, false},
		{"01:00:00", 3600, false},
		{"01:02:03", 3723, false},
		{"23:59:59", 86399, false},

		// DD-HH:MM:SS format
		{"1-00:00:00", 86400, false},
		{"2-01:02:03", 176523, false},
		{"7-12:30:45", 649845, false},

		// Edge cases
		{"00:00", 0, false},
		{"0-00:00:00", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseEtime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseEtime(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.expected {
				t.Errorf("parseEtime(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseProcessTable(t *testing.T) {
	t.Parallel()
	out := `  PID  PPID TTY      COMM     ELAPSED
    1     0 ??       launchd  10-00:00:00
  420    1  ??       claude   01:02:03
  500   420 ttys001  claude   00:05:00

  not-a-pid     1 ?? claude 00:01:00
  600     1 ??       claude
`
	entries := parseProcessTable(out)
	if len(entries) != 3 {
		t.Fatalf("parseProcessTable() returned %d entries, want 3: %+v", len(entries), entries)
	}
	want := []processEntry{
		{PID: 1, PPID: 0, TTY: "??", Comm: "launchd", Etime: "10-00:00:00"},
		{PID: 420, PPID: 1, TTY: "??", Comm: "claude", Etime: "01:02:03"},
		{PID: 500, PPID: 420, TTY: "ttys001", Comm: "claude", Etime: "00:05:00"},
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], w)
		}
	}
}

// psTable builds the raw text a ps -eo pid,ppid,tty,comm,etime call would
// print, so the ownership rules can be exercised without real processes.
func psTable(rows ...string) string {
	return "  PID  PPID TTY      COMM     ELAPSED\n" + strings.Join(rows, "\n") + "\n"
}

// candidatePIDs reduces candidate rows to the pid set the assertions care about.
func candidatePIDs(candidates []unownedCandidate) map[int]int {
	got := make(map[int]int, len(candidates))
	for _, c := range candidates {
		got[c.PID] = c.PPID
	}
	return got
}

// TestUnownedCandidates_Ownership is the gt-h1tq regression: the orphan scan
// used to signal any TTY-less claude with a cwd under the town, including a
// headless `claude -p` child of a live `om review`. Only processes nobody owns
// may be signaled.
func TestUnownedCandidates_Ownership(t *testing.T) {
	t.Parallel()
	entries := parseProcessTable(psTable(
		// (a) reparented to launchd — a genuine orphan
		"  100     1 ??       claude   01:00:00",
		// (b) live parent (pid 150, listed below) — the `om review` backend case
		"  200   150 ??       claude   01:00:00",
		// the live parent: itself a claude, owned by the shell at 900
		"  150   900 ??       claude   02:00:00",
		"  900     1 ttys002  zsh      03:00:00",
		// (c) parent pid absent from the table — parent already exited
		"  300   250 ??       claude   01:00:00",
		// (d) ppid 0 (kernel) is not an owner either
		"  400     0 ??       claude   01:00:00",
	))

	got := candidatePIDs(unownedCandidates(entries, nil))

	for _, pid := range []int{100, 300, 400} {
		if _, ok := got[pid]; !ok {
			t.Errorf("PID %d: not a candidate, want candidate (nobody owns it)", pid)
		}
	}
	for _, pid := range []int{200, 150} {
		if _, ok := got[pid]; ok {
			t.Errorf("PID %d: candidate, want not a candidate (live parent in the table)", pid)
		}
	}
}

// TestUnownedCandidates_StrandedTmuxSurvivor is the gt-tbtx regression: a
// pane process forks directly under the tmux server, so once its session is
// killed a survivor that ignored the resulting SIGHUP keeps the
// still-running server as its ppid. The server hosts unrelated sessions and
// says nothing about whether THIS process is still owned, so it must not
// count as a live parent the way the gt-h1tq `om review` case does.
func TestUnownedCandidates_StrandedTmuxSurvivor(t *testing.T) {
	t.Parallel()
	entries := parseProcessTable(psTable(
		// tmux server: alive, hosting other (unrelated) sessions
		"   50     1 ??       tmux     05:00:00",
		// stranded survivor of a session the server already killed
		"  210    50 ??       claude   01:00:00",
	))

	got := candidatePIDs(unownedCandidates(entries, nil))

	if _, ok := got[210]; !ok {
		t.Error("PID 210 (survivor parented to the tmux server): want candidate")
	}
}

// TestUnownedCandidates_LiveParentChain documents the cascade: a claude whose
// parent is itself a candidate is not signaled while the parent is; the
// parent is signaled this cycle and the child reparents to launchd, so the
// next cycle collects it.
func TestUnownedCandidates_LiveParentChain(t *testing.T) {
	t.Parallel()
	entries := parseProcessTable(psTable(
		"  100     1 ??       claude   01:00:00", // orphaned parent
		"  200   100 ??       claude   00:30:00", // its child, parent still alive
	))

	got := candidatePIDs(unownedCandidates(entries, nil))

	if _, ok := got[100]; !ok {
		t.Error("PID 100 (reparented to launchd): want candidate")
	}
	if _, ok := got[200]; ok {
		t.Error("PID 200 (child of a live parent): want not a candidate this cycle")
	}
}

// TestUnownedCandidates_ExistingFilters pins the filters that were already
// there before the ownership test, so adding ppid did not loosen any of them.
func TestUnownedCandidates_ExistingFilters(t *testing.T) {
	t.Parallel()
	entries := parseProcessTable(psTable(
		"  101     1 ??       claude   01:00:00", // baseline: a candidate
		"  102     1 ttys004  claude   01:00:00", // has a TTY — interactive
		"  103     1 ??       node     01:00:00", // not a tracked agent comm
		"  104     1 ??       claude   00:00:30", // younger than minOrphanAge
		"  105     1 ??       claude   01:00:00", // protected by a tmux session
	))

	got := candidatePIDs(unownedCandidates(entries, map[int]bool{105: true}))

	if _, ok := got[101]; !ok {
		t.Error("PID 101: want candidate")
	}
	for pid, why := range map[int]string{
		102: "has a controlling TTY",
		103: "comm is not a tracked agent runtime",
		104: "younger than the 60s floor",
		105: "protected tmux pid",
	} {
		if _, ok := got[pid]; ok {
			t.Errorf("PID %d: candidate, want not a candidate (%s)", pid, why)
		}
	}
}

// TestUnownedCandidates_AgeFloorMatchesParseEtime keeps the candidate age
// (returned to callers for display) in agreement with the ps etime parser.
func TestUnownedCandidates_AgeFloorMatchesParseEtime(t *testing.T) {
	t.Parallel()
	entries := parseProcessTable(psTable("  101     1 ??       claude   01:00:00"))
	candidates := unownedCandidates(entries, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].Age != 3600 {
		t.Errorf("Age = %d, want 3600", candidates[0].Age)
	}
}

// resolveTownRootFromDir finds the town a directory belongs to by walking up
// to mayor/town.json: from the root or any subdirectory, never from a
// sibling town or from outside every town (resolveTownRoot starts it from a
// process's cwd; TestIntegrationGetProcessCwd reads that).
func TestResolveTownRootFromDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	townA := filepath.Join(parent, "gastown")
	townB := filepath.Join(parent, "gt-financing")
	for _, town := range []string{townA, townB} {
		if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	subDir := filepath.Join(townA, "polecats", "test-polecat")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		townA:       townA,
		subDir:      townA,
		townB:       townB,
		parent:      "",
		t.TempDir(): "",
	} {
		if got := resolveTownRootFromDir(dir); got != want {
			t.Errorf("resolveTownRootFromDir(%q) = %q, want %q", dir, got, want)
		}
	}
}
