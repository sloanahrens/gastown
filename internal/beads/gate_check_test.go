package beads

import (
	"errors"
	"testing"
)

// plainRecordedBeads is a NewPlain wrapper whose bd calls go to rec: the
// wrapper kind the patrol's gate check builds, where bd's stderr survives.
func plainRecordedBeads(dir string, rec *recorder) *Beads {
	b := NewPlain(dir, nil)
	b.exec = rec.exec
	return b
}

// The gate check carries each type to bd as its own --type flag on `bd gate
// check` itself (gt-p85pw.8).
func TestGateCheckPinsTheArgv(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stderr: "No open gates of type 'gh' found.\n"} })
	if _, err := plainRecordedBeads(t.TempDir(), rec).GateCheck("gh:run", "gh:pr"); err != nil {
		t.Fatalf("GateCheck: %v", err)
	}
	argvs := rec.argvs()
	if len(argvs) != 1 || argvs[0] != "gate check --type=gh:run --type=gh:pr" {
		t.Fatalf("bd argv = %q, want one `gate check --type=gh:run --type=gh:pr`", argvs)
	}
}

// The seam hands the patrol the line bd reported, with the stray JSON null bd
// prints ahead of its notice stripped once, here: bd writes notices to stderr
// in machine mode, and the no-gates notice is not worth a log line.
func TestGateCheckReportsTheLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"resolved gate", "", "Resolved gate gt-1 (timer)\n", "Resolved gate gt-1 (timer)"},
		{"null before the resolved gate", "", "nullResolved gate gt-1 (timer)\n", "Resolved gate gt-1 (timer)"},
		{"null before the no-gates notice", "", "nullNo open gates of type 'timer' found.\n", ""},
		{"no-gates notice", "", "No open gates of type 'timer' found.\n", ""},
		{"last of several notices", "", "Resolved gate gt-1 (timer)\nResolved gate gt-2 (timer)\n", "Resolved gate gt-2 (timer)"},
		{"report on stdout", "Resolved gate gt-2 (timer)\n", "", "Resolved gate gt-2 (timer)"},
		{"stderr warning does not hide the stdout report", "Resolved gate gt-3 (timer)\n", "Warning: .beads has permissions 0755\n", "Resolved gate gt-3 (timer)"},
		{"bare envelope", `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":null}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: tc.stdout, stderr: tc.stderr} })
			got, err := plainRecordedBeads(t.TempDir(), rec).GateCheck("timer")
			if err != nil {
				t.Fatalf("GateCheck: %v", err)
			}
			if got != tc.want {
				t.Errorf("GateCheck = %q, want %q", got, tc.want)
			}
		})
	}
}

// A failed gate check reports the failure, never an empty answer that reads
// as "nothing was resolved" (gt-oyrav): the patrol keeps a database whose
// check failed due for the next tick.
func TestGateCheckReportsBdFailure(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stderr: "Error: dolt is down\n", err: exitError{1}} })
	got, err := plainRecordedBeads(t.TempDir(), rec).GateCheck("timer")
	if err == nil {
		t.Fatal("a failed gate check returned no error")
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Errorf("err = %T %v, want *CLIError", err, err)
	}
	if got != "" {
		t.Errorf("GateCheck = %q on failure, want empty", got)
	}
}
