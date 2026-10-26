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
		{"quiet with nothing written", "quiet", "", false},
		{"quiet with a line", "quiet", "done\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := machineStdoutProblem(tc.mode, []byte(tc.stdout))
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
