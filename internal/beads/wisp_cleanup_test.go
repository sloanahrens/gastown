package beads

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// fakeWispMachineTreeBody is fakeWispTreeBody answered the way bd 1.2.2 does
// under BD_MACHINE=1: the flat child lists sit in the envelope's data member,
// with pagination and error null (gt-8f6m4).
const fakeWispMachineTreeBody = `if [ "$1" = "show" ]; then
  case "$2" in
    gt-wisp-root)
      echo '{"schema_version":1,"contract_version":1,"data":{"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"},{"id":"gt-wisp-root.2","status":"closed"}]},"pagination":null,"error":null}'
      exit 0
      ;;
    gt-wisp-root.1)
      echo '{"schema_version":1,"contract_version":1,"data":{"gt-wisp-root.1":[{"id":"gt-wisp-root.1.1","status":"in_progress"}]},"pagination":null,"error":null}'
      exit 0
      ;;
  esac
  echo '{"schema_version":1,"contract_version":1,"data":{"gt-wisp-root.2":[]},"pagination":null,"error":null}'
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

// TestWispTree_ReadsMachineEnvelope reproduces the production read: wispCmd
// runs bd in machine mode, so bd 1.2.2 answers `show --children --json` with
// the envelope rather than the flat child lists. Unwrapped, every child must
// still come back; a regression to parsing the envelope directly reports no
// children or errors, and the patrol closes only the root (gt-8f6m4).
//
// The fake is a shell bd, like every other wisp test in this file (gt-da2x):
// the subprocess path is what is under test — machine mode in the environment
// and the Cmd layer's unwrapping — so an in-process seam would replace the
// code this test exists to cover.
func TestWispTree_ReadsMachineEnvelope(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeFakeBd(t, t.TempDir(), logPath, fakeWispMachineTreeBody)

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

// machineChildrenEnvelope is the shape `BD_MACHINE=1 bd show <id> --children
// --json` printed on bd 1.2.2 (gt-8f6m4).
const machineChildrenEnvelope = `{"schema_version":1,"contract_version":1,` +
	`"data":{"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"}]},` +
	`"pagination":null,"error":null}`

// machineChildrenEnvelopeShuffled is the same envelope with its members
// written in a different order, so a case using it shows the parser's answer
// comes from its sorted traversal rather than the bytes' order.
const machineChildrenEnvelopeShuffled = `{"error":null,"data":{"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"}]},` +
	`"pagination":null,"schema_version":1,"contract_version":1}`

// TestParseChildrenJSON_RefusesUnknownShape pins the parser's contract: the
// flat payload is still read, and any object member that is not a child list —
// a number, an object, null, or bd's machine envelope itself — is an error
// naming the member rather than a silent "no children" (gt-8f6m4). Members
// are visited in sorted order, so the cases that put the envelope's other
// members first and last in the bytes answer identically.
func TestParseChildrenJSON_RefusesUnknownShape(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantIDs []string
		wantErr bool
		wantKey string // member the error must name
	}{
		{
			name:    "flat shape parses, metadata last",
			body:    `{"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"}],"schema_version":1}`,
			wantIDs: []string{"gt-wisp-root.1"},
		},
		{
			name:    "flat shape parses, metadata first",
			body:    `{"schema_version":1,"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"}]}`,
			wantIDs: []string{"gt-wisp-root.1"},
		},
		{
			name: "schema_version alone is no children",
			body: `{"schema_version":1}`,
		},
		{
			name:    "number member is refused",
			body:    `{"contract_version":1}`,
			wantErr: true,
			wantKey: "contract_version",
		},
		{
			name:    "null member is refused",
			body:    `{"gt-wisp-root":null}`,
			wantErr: true,
			wantKey: "gt-wisp-root",
		},
		{
			name:    "object member is refused",
			body:    `{"gt-wisp-root":{"gt-wisp-root.1":[]}}`,
			wantErr: true,
			wantKey: "gt-wisp-root",
		},
		{
			name:    "array of non-issues is refused",
			body:    `{"gt-wisp-root":[1,2]}`,
			wantErr: true,
			wantKey: "gt-wisp-root",
		},
		{
			name:    "two child lists are refused",
			body:    `{"gt-wisp-a":[],"gt-wisp-b":[]}`,
			wantErr: true,
			wantKey: "2 child lists",
		},
		{
			name:    "unknown member refused beside a child list",
			body:    `{"a-null":null,"gt-wisp-root":[{"id":"gt-wisp-root.1","status":"open"}]}`,
			wantErr: true,
			wantKey: "a-null",
		},
		{
			name:    "machine envelope is refused",
			body:    machineChildrenEnvelope,
			wantErr: true,
			wantKey: "contract_version", // first member in sorted order
		},
		{
			name:    "machine envelope is refused, members in another order",
			body:    machineChildrenEnvelopeShuffled,
			wantErr: true,
			wantKey: "contract_version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			children, err := parseChildrenJSON([]byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseChildrenJSON = %v, nil; want an error", childIDs(children))
				}
				if !strings.Contains(err.Error(), tt.wantKey) {
					t.Fatalf("error %q does not name %q", err, tt.wantKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseChildrenJSON: %v", err)
			}
			if got := childIDs(children); !slices.Equal(got, tt.wantIDs) {
				t.Fatalf("parseChildrenJSON = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

// TestParseChildrenMapJSON_ChildlessParent covers what a multi-parent read
// does with a parent that has no children. Measured on bd 1.2.2, that parent
// comes back as [] (processIssue initializes every resolved parent's list
// before reading its dependents), so ChildrenOf must read it, find no
// children, and drop it — not refuse the whole payload (gt-8f6m4).
func TestParseChildrenMapJSON_ChildlessParent(t *testing.T) {
	const withChildren = `{"id":"gt-wisp-a.1","status":"open"}`
	for _, tt := range []struct{ name, body string }{
		{"metadata last", `{"gt-wisp-a":[` + withChildren + `],"gt-wisp-b":[],"schema_version":1}`},
		{"metadata first", `{"schema_version":1,"gt-wisp-b":[],"gt-wisp-a":[` + withChildren + `]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChildrenMapJSON([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseChildrenMapJSON: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("parseChildrenMapJSON = %v, want only the parent with children", got)
			}
			if children := got["gt-wisp-a"]; len(children) != 1 || children[0].ID != "gt-wisp-a.1" {
				t.Fatalf("gt-wisp-a = %v, want its one child", children)
			}
			if children, ok := got["gt-wisp-b"]; ok {
				t.Fatalf("childless gt-wisp-b kept: %v", children)
			}
		})
	}
}

// TestParseChildrenMapJSON_RefusesUnknownShape covers the same rule on the
// multi-parent reader: bd's envelope members that carry no child list are
// errors naming the member, never silently dropped (gt-8f6m4).
func TestParseChildrenMapJSON_RefusesUnknownShape(t *testing.T) {
	for _, tt := range []struct{ name, body, wantKey string }{
		{"machine envelope", machineChildrenEnvelope, "contract_version"},
		{"null member", `{"gt-wisp-root":null}`, "gt-wisp-root"},
		{"number member", `{"contract_version":1}`, "contract_version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseChildrenMapJSON([]byte(tt.body)); err == nil || !strings.Contains(err.Error(), tt.wantKey) {
				t.Fatalf("parseChildrenMapJSON(%s) error = %v, want an error naming %q", tt.body, err, tt.wantKey)
			}
		})
	}
}

func childIDs(issues []*Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func containsCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}
