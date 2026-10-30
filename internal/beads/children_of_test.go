package beads

import (
	"slices"
	"strings"
	"testing"
)

// bd 1.2.2 writes a childless parent as [], never null (measured 2026-10-29:
// cmd/bd/show_children.go initializes every resolved parent's list before it
// reads the dependents). A null member is therefore not a shape bd emits, and
// the reads must refuse it rather than count it as no children (gt-8f6m4).
const childrenOfTestChild = `{"id":"gt-wisp-a.1","title":"A.1","status":"open","dependency_type":"parent-child"}`

func TestChildrenOf_ChildlessAndNullMembers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stdout  string
		wantErr string // non-empty: the error must name this member
	}{
		{
			name:   "childless parent is an empty list and is dropped",
			stdout: `{"gt-wisp-a":[` + childrenOfTestChild + `],"gt-wisp-b":[],"schema_version":1}`,
		},
		{
			name:    "null member is refused",
			stdout:  `{"gt-wisp-a":[` + childrenOfTestChild + `],"gt-wisp-b":null,"schema_version":1}`,
			wantErr: "gt-wisp-b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: tt.stdout} })
			got, err := newRecordedBeads(t.TempDir(), rec).ChildrenOf("gt-wisp-a", "gt-wisp-b")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ChildrenOf error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ChildrenOf: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ChildrenOf = %v, want only gt-wisp-a", got)
			}
			if children := got["gt-wisp-a"]; len(children) != 1 || children[0].ID != "gt-wisp-a.1" {
				t.Errorf("gt-wisp-a = %v, want its one child", children)
			}
		})
	}
}

// Children reads one parent through the same parser. A childless parent is an
// empty list (no children, no error); a null member is refused (gt-8f6m4).
func TestChildren_SingleParentShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stdout  string
		wantIDs []string
		wantErr string
	}{
		{
			name:    "parent with a child",
			stdout:  `{"gt-wisp-a":[` + childrenOfTestChild + `],"schema_version":1}`,
			wantIDs: []string{"gt-wisp-a.1"},
		},
		{
			name:   "childless parent is no children, not an error",
			stdout: `{"gt-wisp-a":[],"schema_version":1}`,
		},
		{
			name:    "null parent is refused",
			stdout:  `{"gt-wisp-a":null,"schema_version":1}`,
			wantErr: "gt-wisp-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: tt.stdout} })
			children, err := newRecordedBeads(t.TempDir(), rec).Children("gt-wisp-a")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Children error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Children: %v", err)
			}
			var got []string
			for _, c := range children {
				got = append(got, c.ID)
			}
			if !slices.Equal(got, tt.wantIDs) {
				t.Fatalf("Children = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

// ChildrenOf reads a whole molecule level in one machine-mode bd show
// --children, keyed by parent, and drops the parents bd lists with none.
func TestChildrenOf_OneShowForAllParents(t *testing.T) {
	t.Parallel()
	envelope := `{"schema_version":1,"contract_version":1,"data":{` +
		`"gt-wisp-mol":[{"id":"gt-wisp-a","title":"A","status":"closed","dependency_type":"parent-child"},{"id":"gt-wisp-b","title":"B","status":"open","dependency_type":"parent-child"}],` +
		`"gt-wisp-a":[]},"pagination":null,"error":null}`
	rec := newRecorder(func([]string) reply { return reply{stdout: envelope} })
	got, err := newRecordedBeads(t.TempDir(), rec).ChildrenOf("gt-wisp-mol", "gt-wisp-a")
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(got) != 1 || len(got["gt-wisp-mol"]) != 2 || got["gt-wisp-mol"][0].Status != "closed" {
		t.Errorf("ChildrenOf = %v, want the molecule's two steps only", got)
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %q, want one bd show", rec.argvs())
	}
	if argv := strings.Join(calls[0].args, " "); !strings.Contains(argv, "show gt-wisp-mol gt-wisp-a --children --json") {
		t.Errorf("argv = %q", argv)
	}
	if v, ok := lastEnvValue(calls[0].env, "BD_MACHINE"); !ok || v != "1" {
		t.Errorf("BD_MACHINE = %q (set %v), want 1", v, ok)
	}
}
