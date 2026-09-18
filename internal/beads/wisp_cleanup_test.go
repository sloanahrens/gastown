package beads

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeBd installs a mock `bd` in binDir that appends its argv to logPath
// and then runs body. Tests drive the real WispTree/CloseWispTree subprocess
// paths through it, so the assertions cover the code that actually runs in
// production rather than a seam that replaces it (gt-da2x).
func writeFakeBd(t *testing.T, binDir, logPath, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script; skipping on Windows")
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" + body
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// bdCalls returns the argv lines the fake bd recorded, verbatim.
func bdCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading fake bd log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// fakeWispTreeBody answers the `show <id> --children --json` reads for a
// three-level wisp: root -> {.1 open, .2 closed}, .1 -> {.1.1 in_progress}.
// Anything else reports no children.
const fakeWispTreeBody = `if [ "$1" = "show" ]; then
  case "$2" in
    gt-wisp-root)
      echo '{"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"},{"id":"gt-wisp-root.2","status":"closed"}],"schema_version":1}'
      exit 0
      ;;
    gt-wisp-root.1)
      echo '{"gt-wisp-root.1":[{"id":"gt-wisp-root.1.1","status":"in_progress"}],"schema_version":1}'
      exit 0
      ;;
  esac
  echo '{"schema_version":1}'
  exit 0
fi
exit 0
`

func TestFormulaWispIDs_FiltersToAttachedFormula(t *testing.T) {
	tests := []struct {
		name   string
		issues []*Issue
		want   []string
	}{
		{
			name:   "no hooked beads",
			issues: nil,
			want:   nil,
		},
		{
			name:   "hooked bead with no attachment metadata is not a wisp",
			issues: []*Issue{{ID: "hq-task-a", Description: "just a regular hooked task"}},
			want:   nil,
		},
		{
			name:   "hooked bead with attached_formula is a wisp",
			issues: []*Issue{{ID: "wisp-a", Description: "attached_formula: mol-dog-reaper\nattached_molecule: wisp-a\n"}},
			want:   []string{"wisp-a"},
		},
		{
			name: "only formula wisps are selected, in query order",
			issues: []*Issue{
				{ID: "hq-task-b", Description: "unrelated work"},
				{ID: "wisp-b", Description: "attached_formula: mol-dog-backup\n"},
				{ID: "wisp-c", Description: "attached_molecule: wisp-c\nattached_at: 2026-01-01T00:00:00Z\n"},
				{ID: "wisp-d", Description: "attached_formula: mol-polecat-work\n"},
			},
			want: []string{"wisp-b", "wisp-d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormulaWispIDs(tt.issues)
			if len(got) != len(tt.want) {
				t.Fatalf("FormulaWispIDs() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("FormulaWispIDs() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestFirstFormulaWisp(t *testing.T) {
	if got := FirstFormulaWisp(nil); got != nil {
		t.Errorf("FirstFormulaWisp(nil) = %v, want nil", got)
	}
	if got := FirstFormulaWisp([]*Issue{{ID: "hq-task", Description: "no metadata"}}); got != nil {
		t.Errorf("FirstFormulaWisp(no metadata) = %v, want nil", got)
	}
	issues := []*Issue{
		{ID: "hq-task-b", Description: "unrelated work"},
		{ID: "wisp-b", Description: "attached_formula: mol-dog-backup\n"},
		{ID: "wisp-d", Description: "attached_formula: mol-polecat-work\n"},
	}
	got := FirstFormulaWisp(issues)
	if got == nil || got.ID != "wisp-b" {
		t.Fatalf("FirstFormulaWisp() = %v, want wisp-b", got)
	}
}

// TestWispTree_WalksAllEphemeralLevels covers the read the daemon and
// `gt dog done` both stand on. It asserts on the argv the fake bd received
// because the whole point of this helper is that it uses
// `show <id> --children --json`: `bd children` (an alias for `bd list
// --parent`) returns an empty list for ephemeral wisp children — verified
// against a live 11-step wisp while fixing gt-da2x — so a regression to it
// would silently report every wisp as having no steps.
func TestWispTree_WalksAllEphemeralLevels(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBd(t, t.TempDir(), logPath, fakeWispTreeBody)

	tree, err := WispTree(context.Background(), t.TempDir(), os.Environ(), "gt-wisp-root")
	if err != nil {
		t.Fatalf("WispTree: %v", err)
	}

	want := []WispStep{
		{ID: "gt-wisp-root"},
		{ID: "gt-wisp-root.1", Status: "open"},
		{ID: "gt-wisp-root.2", Status: "closed"},
		{ID: "gt-wisp-root.1.1", Status: "in_progress"},
	}
	if len(tree) != len(want) {
		t.Fatalf("WispTree() = %+v, want %+v", tree, want)
	}
	for i := range want {
		if tree[i] != want[i] {
			t.Fatalf("WispTree()[%d] = %+v, want %+v (full: %+v)", i, tree[i], want[i], tree)
		}
	}

	calls := bdCalls(t, logPath)
	for _, want := range []string{
		"show gt-wisp-root --children --json",
		"show gt-wisp-root.1 --children --json",
		"show gt-wisp-root.1.1 --children --json",
	} {
		if !containsCall(calls, want) {
			t.Errorf("fake bd calls %q missing; got %q", want, calls)
		}
	}
}

// TestWispTree_StopsOnCycles guards the seen-set: a malformed tree whose child
// points back at an ancestor must terminate instead of looping until the query
// timeout.
func TestWispTree_StopsOnCycles(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBd(t, t.TempDir(), logPath, `if [ "$1" = "show" ]; then
  case "$2" in
    gt-wisp-cycle)
      echo '{"gt-wisp-cycle":[{"id":"gt-wisp-child","status":"open"}],"schema_version":1}'
      exit 0
      ;;
    gt-wisp-child)
      echo '{"gt-wisp-child":[{"id":"gt-wisp-cycle","status":"open"}],"schema_version":1}'
      exit 0
      ;;
  esac
  echo '{"schema_version":1}'
  exit 0
fi
exit 0
`)

	tree, err := WispTree(context.Background(), t.TempDir(), os.Environ(), "gt-wisp-cycle")
	if err != nil {
		t.Fatalf("WispTree: %v", err)
	}
	if len(tree) != 2 {
		t.Fatalf("WispTree() = %+v, want the cycle visited exactly once (2 steps)", tree)
	}
}

// TestWispTree_ErrorsOnChildReadFailure pins the fail-safe: a partial tree
// would let the caller close a parent while its children survive, so a failed
// child read must surface as an error.
func TestWispTree_ErrorsOnChildReadFailure(t *testing.T) {
	writeFakeBd(t, t.TempDir(), filepath.Join(t.TempDir(), "bd.log"), "echo 'dolt unreachable' >&2\nexit 1\n")

	if _, err := WispTree(context.Background(), t.TempDir(), os.Environ(), "gt-wisp-root"); err == nil {
		t.Fatal("WispTree() error = nil, want error when the child read fails")
	}
}

// TestCloseWispTree_DeepestFirstAndSkipsClosed covers the write path: every
// open step is force-closed in one call, children before their parent
// (gt-7lx3), and steps that are already closed are left out.
func TestCloseWispTree_DeepestFirstAndSkipsClosed(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBd(t, t.TempDir(), logPath, fakeWispTreeBody)

	ctx := context.Background()
	dir := t.TempDir()
	tree, err := WispTree(ctx, dir, os.Environ(), "gt-wisp-root")
	if err != nil {
		t.Fatalf("WispTree: %v", err)
	}

	closed, err := CloseWispTree(ctx, dir, os.Environ(), "abandoned: idle dog, no step progress", tree)
	if err != nil {
		t.Fatalf("CloseWispTree: %v", err)
	}
	if closed != 3 {
		t.Errorf("CloseWispTree() closed = %d, want 3 (root, .1, .1.1 — not the closed .2)", closed)
	}

	want := "close gt-wisp-root.1.1 gt-wisp-root.1 gt-wisp-root --force --reason abandoned: idle dog, no step progress"
	if !containsCall(bdCalls(t, logPath), want) {
		t.Errorf("fake bd calls %q missing; got %q", want, bdCalls(t, logPath))
	}
}

// TestCloseWispTree_SkipsAlreadyClosedSteps confirms steps that are already
// closed are left out of the close call — bd close on a closed bead is at
// best a wasted write — while the still-open root is still closed.
func TestCloseWispTree_SkipsAlreadyClosedSteps(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBd(t, t.TempDir(), logPath, "exit 0\n")

	tree := []WispStep{{ID: "gt-wisp-root"}, {ID: "gt-wisp-root.1", Status: "closed"}}
	closed, err := CloseWispTree(context.Background(), t.TempDir(), os.Environ(), "dog done", tree)
	if err != nil {
		t.Fatalf("CloseWispTree: %v", err)
	}
	if closed != 1 {
		t.Errorf("CloseWispTree() closed = %d, want 1 (only .1 is closed; the root was still open)", closed)
	}
	if !containsCall(bdCalls(t, logPath), "close gt-wisp-root --force --reason dog done") {
		t.Errorf("fake bd calls = %q, want a single close of the root", bdCalls(t, logPath))
	}
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}
