package beads

import (
	"reflect"
	"testing"
)

// argsAfterCommand strips the leading global flags (--allow-stale) a recorded
// call may carry, leaving the verb and its arguments.
func argsAfterCommand(args []string) []string {
	for i, a := range args {
		if a == cmdOf(args) {
			return args[i:]
		}
	}
	return nil
}

// TestMaintenanceVerbsRunOneMachineModeBdCall pins the bd verb each
// maintenance write uses (gt-fcxe9.12): these replace raw SQL, so each must
// be exactly one bd call under BD_MACHINE=1 and never "bd sql".
func TestMaintenanceVerbsRunOneMachineModeBdCall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		call func(b *Beads) error
		want []string
	}{
		{"DeleteIssues", func(b *Beads) error { return b.DeleteIssues("gt-a", "gt-b") }, []string{"delete", "gt-a", "gt-b", "--force"}},
		{"DemoteToWisp", func(b *Beads) error { return b.DemoteToWisp("gt-a") }, []string{"update", "gt-a", "--ephemeral"}},
		{"ReopenUnassigned", func(b *Beads) error { return b.ReopenUnassigned("gt-a") }, []string{"update", "gt-a", "--status=open", "--assignee="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(nil)
			b := NewIsolated(t.TempDir())
			b.exec = r.exec
			if err := tc.call(b); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			calls := r.calls()
			if len(calls) != 1 {
				t.Fatalf("%s ran %d bd calls, want 1: %v", tc.name, len(calls), calls)
			}
			if got := argsAfterCommand(calls[0].args); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s argv = %q, want %q", tc.name, got, tc.want)
			}
			if v, ok := lastEnvValue(calls[0].env, "BD_MACHINE"); !ok || v != "1" {
				t.Errorf("%s ran without BD_MACHINE=1", tc.name)
			}
		})
	}
}

// TestMaintenanceVerbsReturnBdRefusal: a machine-mode refusal (exit 21) is an
// error, never a silent success.
func TestMaintenanceVerbsReturnBdRefusal(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{
			stdout: `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"refused","message":"read-only"}}`,
			err:    exitError{21},
		}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec
	if err := b.DeleteIssues("gt-a"); err == nil {
		t.Error("DeleteIssues returned nil on a refused delete")
	}
	if err := b.DemoteToWisp("gt-a"); err == nil {
		t.Error("DemoteToWisp returned nil on a refused update")
	}
	if err := b.ReopenUnassigned("gt-a"); err == nil {
		t.Error("ReopenUnassigned returned nil on a refused update")
	}
}

func TestDeleteIssuesOfNothingRunsNothing(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := NewIsolated(t.TempDir())
	b.exec = r.exec
	if err := b.DeleteIssues(); err != nil {
		t.Fatal(err)
	}
	if n := len(r.calls()); n != 0 {
		t.Errorf("DeleteIssues() ran %d bd calls, want 0", n)
	}
}
