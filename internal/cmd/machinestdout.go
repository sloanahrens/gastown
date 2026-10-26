package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// machineStdoutProblem reports what a command given --json or --quiet wrote to stdout that a
// machine reader cannot take: with --json, a line that is not JSON (one JSON value, or JSON lines);
// with --quiet, anything at all. Empty output passes either way. mode is "json" or "quiet". The
// error names the rule, not the content: captured stdout can hold a secret (gt-b9j3o.7).
func machineStdoutProblem(mode string, stdout []byte) error {
	body := bytes.TrimSpace(stdout)
	if len(body) == 0 {
		return nil
	}
	switch mode {
	case "quiet":
		return fmt.Errorf("--quiet writes nothing to stdout, but it wrote output")
	case "json":
		if !jsonOnly(body) {
			return fmt.Errorf("--json writes only JSON to stdout, but it wrote a line that is not JSON")
		}
	default:
		return fmt.Errorf("machineStdoutProblem: unknown mode %q", mode)
	}
	return nil
}

// jsonOnly reports whether every line of body is one JSON value: a single document, or JSON lines.
func jsonOnly(body []byte) bool {
	if json.Valid(body) {
		return true
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) != "" && !json.Valid([]byte(line)) {
			return false
		}
	}
	return true
}

// machineModes lists the machine-output flags of cmd, in the order json then quiet, as the names
// machineStdoutProblem takes. A command with neither is not a machine reader's command.
func machineModes(cmd *cobra.Command) []string {
	var modes []string
	for _, name := range []string{"json", "quiet"} {
		if f := cmd.Flags().Lookup(name); f != nil {
			modes = append(modes, name)
		}
	}
	return modes
}

// checkMachineCommand runs cmd through run, with each machine-output flag set in turn, and returns
// the first problem with its stdout. run is an injected runner: nothing here runs the real
// commands. Wiring the check into presubmit, so the real --json and --quiet commands run through
// it, is gt-keakj.
func checkMachineCommand(cmd *cobra.Command, run func(args []string) ([]byte, error)) error {
	for _, mode := range machineModes(cmd) {
		stdout, err := run([]string{"--" + mode})
		if err != nil {
			return fmt.Errorf("%s --%s: %w", cmd.Name(), mode, err)
		}
		if err := machineStdoutProblem(mode, stdout); err != nil {
			return fmt.Errorf("%s: %w", cmd.Name(), err)
		}
	}
	return nil
}

// CheckMachineStdout reports whether captured stdout breaks the machine-readable contract of a
// command run with --json (one JSON value, or JSON lines) or --quiet (nothing). It is the check the
// presubmit's machine-stdout stage applies to each real command's output (gt-keakj).
func CheckMachineStdout(mode string, stdout []byte) error {
	return machineStdoutProblem(mode, stdout)
}

// MachineCommand is one command that takes a machine-output flag: its path below the root and the
// flags (json, quiet) it defines.
type MachineCommand struct {
	Path  []string
	Modes []string
}

// MachineCommands lists every leaf command in the gt tree that defines --json or --quiet, in tree
// order. A group command (one with subcommands) is not listed: running it prints help, which is not
// machine output, so its own flags have no meaning there and its subcommands are the ones checked. Hidden
// commands are included: they are still a machine reader's command if they print.
func MachineCommands() []MachineCommand {
	var out []MachineCommand
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		if modes := machineModes(c); len(modes) > 0 && len(path) > 0 && !c.HasSubCommands() {
			out = append(out, MachineCommand{Path: path, Modes: modes})
		}
		for _, sub := range c.Commands() {
			walk(sub, append(append([]string{}, path...), sub.Name()))
		}
	}
	walk(rootCmd, nil)
	return out
}
