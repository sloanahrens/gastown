package beads

import (
	"strings"
	"testing"
)

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
