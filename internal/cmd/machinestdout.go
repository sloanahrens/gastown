package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// machineStdoutProblem reports what a command given --json or --quiet wrote to stdout that a
// machine reader cannot take: with --json, anything that is not one JSON value; with --quiet,
// anything at all. Empty output passes either way. mode is "json" or "quiet" (gt-b9j3o.7).
func machineStdoutProblem(mode string, stdout []byte) error {
	body := bytes.TrimSpace(stdout)
	if len(body) == 0 {
		return nil
	}
	first, _, _ := strings.Cut(string(body), "\n")
	switch mode {
	case "quiet":
		return fmt.Errorf("--quiet writes nothing to stdout, but stdout starts %q", first)
	case "json":
		if !json.Valid(body) {
			return fmt.Errorf("--json writes only JSON to stdout, but stdout starts %q", first)
		}
	default:
		return fmt.Errorf("machineStdoutProblem: unknown mode %q", mode)
	}
	return nil
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
// the first problem with its stdout. run is the in-process runner a test injects; the real
// command runs through the same path, so the check needs no process of its own.
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
