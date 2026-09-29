package beads

import (
	"fmt"
	"strings"
	"testing"
)

func readyBoardEnvelope(n int, truncated bool) string {
	var items []string
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(`{"id":"gt-board-%d","title":"t","status":"open","priority":2,"issue_type":"task"}`, i))
	}
	return fmt.Sprintf(`{"schema_version":1,"contract_version":1,"data":[%s],"pagination":{"returned":%d,"truncated":%t},"error":null}`,
		strings.Join(items, ","), n, truncated)
}

// ReadyAll reads the whole board in one machine-mode call: --limit 0 lifts
// bd's default page of 100, and the envelope's pagination says whether bd
// cut it anyway (gt-59o9, gt-7iwy0.2).
func TestReadyAll_ReadsWholeBoardInMachineMode(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func(args []string) reply { return reply{stdout: readyBoardEnvelope(373, false)} })
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyAll()
	if err != nil {
		t.Fatalf("ReadyAll: %v", err)
	}
	if len(issues) != 373 || issues[0].ID != "gt-board-0" {
		t.Fatalf("ReadyAll = %d issues (first %v), want the 373-bead board in order", len(issues), issues)
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %q, want one bd ready", rec.argvs())
	}
	argv := strings.Join(calls[0].args, " ")
	for _, want := range []string{"ready --json", "--limit 0", "--exclude-label", "--exclude-type"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q lacks %q", argv, want)
		}
	}
	if v, ok := lastEnvValue(calls[0].env, "BD_MACHINE"); !ok || v != "1" {
		t.Errorf("BD_MACHINE = %q (set %v), want 1", v, ok)
	}
}

func TestReadyAll_RefusesAPartialBoard(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"truncated envelope": readyBoardEnvelope(100, true),
		"prose":              "No issues found.\n",
		"empty":              "",
		"envelope error":     `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"store_unavailable","message":"no store"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: out} })
			if issues, err := newRecordedBeads(t.TempDir(), rec).ReadyAll(); err == nil {
				t.Errorf("ReadyAll accepted %s as %d issues", name, len(issues))
			}
		})
	}
}

// A bd from before machine mode ignores BD_MACHINE and prints a bare array;
// --limit 0 already made it whole.
func TestReadyAll_AcceptsLegacyArray(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply {
		return reply{stdout: `[{"id":"gt-a","title":"t","status":"open","priority":2,"issue_type":"task"}]`}
	})
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyAll()
	if err != nil || len(issues) != 1 || issues[0].ID != "gt-a" {
		t.Fatalf("ReadyAll = %v, %v; want [gt-a]", issues, err)
	}
}
