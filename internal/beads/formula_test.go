package beads

import (
	"errors"
	"slices"
	"testing"
)

// TestFormulaListArgv pins each form of the list to bd's argv and mode: the
// JSON form runs in machine mode for the payload, the text form outside it
// for bd's prose (gt-p85pw.11).
func TestFormulaListArgv(t *testing.T) {
	t.Parallel()
	const answer = `[{"name":"mol-x"}]`
	tests := []struct {
		name    string
		call    func(*Beads) ([]byte, error)
		argv    string
		machine bool
	}{
		{"json", (*Beads).FormulaList, "formula list --json", true},
		{"text", (*Beads).FormulaListText, "formula list", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func([]string) reply { return reply{stdout: answer} })
			b := newRecordedBeads(t.TempDir(), r)

			out, err := tc.call(b)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if string(out) != answer {
				t.Errorf("%s = %q, want bd's stdout unchanged", tc.name, out)
			}
			if got, want := r.argvs(), []string{tc.argv}; !slices.Equal(got, want) {
				t.Errorf("argv = %v, want %v", got, want)
			}
			env := r.calls()[0].env
			if _, ok := lastEnvValue(env, "BD_MACHINE"); ok != tc.machine {
				t.Errorf("BD_MACHINE set = %v, want %v", ok, tc.machine)
			}
			if !tc.machine {
				if v, _ := lastEnvValue(env, "BD_READONLY"); v != "true" {
					t.Errorf("BD_READONLY = %q, want true: the verb never writes", v)
				}
			}
		})
	}
}

// TestFormulaShowTextHandsBdTheFormula is gt formula show --raw's promise:
// bd gets `formula show <name>` and prints its own prose.
func TestFormulaShowTextHandsBdTheFormula(t *testing.T) {
	t.Parallel()
	const printed = "📐 mol-x\n   Type: task\n"
	r := newRecorder(func([]string) reply { return reply{stdout: printed} })
	b := newRecordedBeads(t.TempDir(), r)

	out, err := b.FormulaShowText("mol-x")
	if err != nil {
		t.Fatalf("FormulaShowText: %v", err)
	}
	if string(out) != printed {
		t.Errorf("FormulaShowText = %q, want bd's stdout unchanged", out)
	}
	if got, want := r.argvs(), []string{"formula show mol-x"}; !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if _, ok := lastEnvValue(r.calls()[0].env, "BD_MACHINE"); ok {
		t.Error("BD_MACHINE must be absent: --raw prints bd's prose, not the envelope")
	}
}

// A failed formula verb reads as bd's own message, taken from the machine
// envelope's typed error as well as the legacy {"error": "..."} string; any
// other output keeps the error as it was.
func TestWithBDMessage(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 1")
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"machine envelope", `{"schema_version":1,"contract_version":1,"data":null,"error":{"kind":"internal","message":"Error 1213 (40001): serialization failure"}}`, "Error 1213 (40001): serialization failure"},
		{"legacy string", `{"error":"creating wisp: boom"}`, "creating wisp: boom"},
		{"other shape", "not json\n", "bd cook x: exit status 1"},
		{"empty", "", "bd cook x: exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := withBDMessage(&unavailableError{msg: "bd cook x: exit status 1", cause: cause, stdout: []byte(tt.out)})
			if err.Error() != tt.want {
				t.Fatalf("message = %q, want %q", err.Error(), tt.want)
			}
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
				t.Fatalf("%v lost its causes", err)
			}
		})
	}
}
