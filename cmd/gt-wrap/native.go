package main

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// omTool, bdTool and gtTool are the tools the config key native accepts: a call of
// om that reads the caller's worktree runs on the Mac, so does a bd hook, which
// reads the Mac's commit message file, and so does a gt call in the host command
// set, which prints or reads what only the Mac has. No other tool does.
const (
	omTool = "om"
	bdTool = "bd"
	// bdNativeSubcommand is the only bd subcommand a native entry may name. The
	// internal Covers rule narrows it to hooks run.
	bdNativeSubcommand = "hooks"
)

// nativeEntry is one tool under the native key of the config file.
type nativeEntry struct {
	Path        string   `json:"path"`
	Subcommands []string `json:"subcommands"`
}

// nativeTools reads the native key of the config file at path. A key that names
// any tool but om, bd and gt refuses the file: the key is not a way to run another
// tool on the Mac. A bd entry may name only hooks, and a gt entry may name only a
// command path in the host command set, so the file cannot make any other call
// native. An entry with no subcommand refuses too, since it would run nothing. The
// path is checked at each call, so a call the path cannot serve is refused where
// it happens, and --doctor reports the same check.
func nativeTools(entries map[string]nativeEntry, path string) (map[string]gtwrap.Native, error) {
	out := make(map[string]gtwrap.Native, len(entries))
	for tool, entry := range entries {
		if tool != omTool && tool != bdTool && tool != gtTool {
			return nil, fmt.Errorf("gtwrap: the target config file %s names native.%s; only %q, %q and %q run natively", path, tool, omTool, bdTool, gtTool)
		}
		if len(entry.Subcommands) == 0 {
			return nil, fmt.Errorf("gtwrap: native.%s.subcommands in the target config file %s names no subcommand; list the subcommand the native run takes, e.g. [\"review\"]", tool, path)
		}
		if tool == bdTool {
			for _, sub := range entry.Subcommands {
				if sub != bdNativeSubcommand {
					return nil, fmt.Errorf("gtwrap: native.%s.subcommands in the target config file %s names %q; bd runs natively only for %q, so list %q alone", tool, path, sub, bdNativeSubcommand, bdNativeSubcommand)
				}
			}
		}
		if tool == gtTool {
			for _, sub := range entry.Subcommands {
				if !gtwrap.GTHostCommand(sub) {
					return nil, fmt.Errorf("gtwrap: native.%s.subcommands in the target config file %s names %q; gt runs natively only for the host command set: %s", tool, path, sub, gtwrap.GTHostCommandList())
				}
			}
		}
		out[tool] = gtwrap.Native{Tool: tool, Path: entry.Path, Subcommands: entry.Subcommands}
	}
	return out, nil
}

// checkNative is every refusal a native path can earn: it cannot run, or it is
// this wrapper. The call path and --doctor both ask it, so they cannot differ.
func checkNative(nat gtwrap.Native, stat func(string) (fs.FileInfo, error), executable func() (string, error)) error {
	if err := nat.Check(stat); err != nil {
		return err
	}
	return nat.CheckNotSelf(stat, executable)
}

// runNative runs a call the config names for the Mac: the configured binary in
// the caller's directory, with the caller's whole environment, and its exit
// status passed through. A path that cannot run refuses the call, and the call
// never reaches the target.
func runNative(nat gtwrap.Native, args []string, s surface) int {
	if err := checkNative(nat, s.stat, s.executable); err != nil {
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
func reportNative(stdout io.Writer, nat gtwrap.Native, stat func(string) (fs.FileInfo, error), executable func() (string, error)) int {
	if err := checkNative(nat, stat, executable); err != nil {
		fmt.Fprintf(stdout, "native: %q (not runnable): %v\n", nat.Path, err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "native: %s (exists)\n", nat.Path)
	return 0
}
