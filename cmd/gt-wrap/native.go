package main

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// omTool is the only tool the config key native accepts: a call of om that
// reads the caller's worktree runs on the Mac, and no other tool does.
const omTool = "om"

// nativeEntry is one tool under the native key of the config file.
type nativeEntry struct {
	Path        string   `json:"path"`
	Subcommands []string `json:"subcommands"`
}

// nativeTools reads the native key of the config file at path. A key that names
// any tool but om refuses the file: the key is not a way to run another tool on
// the Mac. An entry with no subcommand refuses too, since it would run nothing.
// The path is checked at each call, so a call the path cannot serve is refused
// where it happens, and --doctor reports the same check.
func nativeTools(entries map[string]nativeEntry, path string) (map[string]gtwrap.Native, error) {
	out := make(map[string]gtwrap.Native, len(entries))
	for tool, entry := range entries {
		if tool != omTool {
			return nil, fmt.Errorf("gtwrap: the target config file %s names native.%s; only %q runs natively", path, tool, omTool)
		}
		if len(entry.Subcommands) == 0 {
			return nil, fmt.Errorf("gtwrap: native.%s.subcommands in the target config file %s names no subcommand; list the subcommand the native run takes, e.g. [\"review\"]", tool, path)
		}
		out[tool] = gtwrap.Native{Tool: tool, Path: entry.Path, Subcommands: entry.Subcommands}
	}
	return out, nil
}

// runNative runs a call the config names for the Mac: the configured binary in
// the caller's directory, with the caller's whole environment, and its exit
// status passed through. A path that cannot run refuses the call, and the call
// never reaches the target.
func runNative(nat gtwrap.Native, args []string, s surface) int {
	if err := nat.Check(s.stat); err != nil {
		return fail(s.stderr, err)
	}
	if s.native == nil {
		return fail(s.stderr, fmt.Errorf("gtwrap: no runner is wired for the native %s, so the call is refused", nat.Tool))
	}
	dir, err := s.getwd()
	if err != nil {
		return fail(s.stderr, fmt.Errorf("gtwrap: get the working directory to run the native %s in: %w", nat.Tool, err))
	}
	argv := append([]string{nat.Path}, args...)
	exit, err := s.native.Run(argv, s.environ(), dir)
	if err != nil {
		fmt.Fprintln(s.stderr, err)
		if exit == 0 {
			exit = exitFailure
		}
	}
	return exit
}

// reportNative is the native line of --doctor: the path a native call runs, and
// whether it can run. A path that cannot run is down, so the doctor exits 1.
func reportNative(stdout io.Writer, nat gtwrap.Native, stat func(string) (fs.FileInfo, error)) int {
	if err := nat.Check(stat); err != nil {
		fmt.Fprintf(stdout, "native: %q (not runnable): %v\n", nat.Path, err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "native: %s (exists)\n", nat.Path)
	return 0
}
