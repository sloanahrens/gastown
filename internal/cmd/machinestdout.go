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
