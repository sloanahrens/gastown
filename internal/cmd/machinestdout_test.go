package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestMachineStdoutProblem: with --json, stdout must be one JSON value; with --quiet, nothing. A
// notice on stdout fails either way, and empty output passes (gt-b9j3o.7).
func TestMachineStdoutProblem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mode   string
		stdout string
		fails  bool
	}{
		{"json is one value", "json", `{"ok":true}`, false},
		{"json array with spaces", "json", "[1, 2]\n", false},
		{"json with nothing written", "json", "", false},
		{"json with a notice first", "json", "note: rebuilding cache\n{\"ok\":true}\n", true},
		{"json with a trailing notice", "json", "{\"ok\":true}\nnote: done\n", true},
		{"json lines, one value a line", "json", "{\"n\":1}\n{\"n\":2}\n", false},
		{"quiet with nothing written", "quiet", "", false},
		{"quiet with a line", "quiet", "done\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := machineStdoutProblem(tc.mode, []byte(tc.stdout))
			if err != nil && strings.Contains(err.Error(), "note:") {
				t.Errorf("the error echoes the captured stdout: %v", err)
			}
			if (err != nil) != tc.fails {
				t.Errorf("machineStdoutProblem(%q, %q) = %v, want a problem: %v", tc.mode, tc.stdout, err, tc.fails)
			}
		})
	}
}

// machineTestCommand builds a command with the given machine flags and no effect of its own.
func machineTestCommand(flags ...string) *cobra.Command {
	cmd := &cobra.Command{Use: "probe"}
	for _, f := range flags {
		cmd.Flags().Bool(f, false, "")
	}
	return cmd
}

// TestCheckMachineCommandRunsEachFlagAndFailsOnANotice: a command with --json whose run writes a
// notice fails the check, a command whose run writes only JSON passes, and a run that errors is
// reported by name. The runner is a function the test passes, so no process starts.
func TestCheckMachineCommandRunsEachFlagAndFailsOnANotice(t *testing.T) {
	t.Parallel()
	jsonOnly := func(args []string) ([]byte, error) {
		if strings.Join(args, " ") == "--json" {
			return []byte(`{"n":1}`), nil
		}
		return nil, nil
	}
	noisy := func(args []string) ([]byte, error) {
		if strings.Join(args, " ") == "--json" {
			return []byte("warning: slow\n{\"n\":1}"), nil
		}
		return nil, nil
	}
	failing := func(args []string) ([]byte, error) { return nil, errors.New("town unreadable") }

	if err := checkMachineCommand(machineTestCommand("json", "quiet"), jsonOnly); err != nil {
		t.Errorf("a JSON-only command failed the check: %v", err)
	}
	if err := checkMachineCommand(machineTestCommand("json"), noisy); err == nil {
		t.Error("a command with a notice on stdout passed the check")
	}
	if err := checkMachineCommand(machineTestCommand("json"), failing); err == nil || !strings.Contains(err.Error(), "town unreadable") {
		t.Errorf("a run error = %v, want it reported by name", err)
	}
	if err := checkMachineCommand(machineTestCommand(), noisy); err != nil {
		t.Errorf("a command with no machine flag was checked: %v", err)
	}
}

// TestMachineStdoutProblemRefusesAnUnknownMode: a mode the checker does not know is an error, not a
// pass, so a new flag cannot skip the check by accident (gt-b9j3o.7).
func TestMachineStdoutProblemRefusesAnUnknownMode(t *testing.T) {
	t.Parallel()
	if err := machineStdoutProblem("yaml", []byte("a: 1")); err == nil {
		t.Error("an unknown mode passed the check")
	}
}

// TestCheckMachineCommandNamesTheCommand: a failure names the command and the flag, so the operator
// knows which command wrote the line (gt-b9j3o.7).
func TestCheckMachineCommandNamesTheCommand(t *testing.T) {
	t.Parallel()
	run := func(args []string) ([]byte, error) { return []byte("note: x\n"), nil }
	err := checkMachineCommand(machineTestCommand("json"), run)
	if err == nil || !strings.Contains(err.Error(), "probe") {
		t.Errorf("the failure = %v, want it to name the command probe", err)
	}
}
