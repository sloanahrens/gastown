// Command tool-updater reports which Homebrew tools are outdated and changes
// nothing. Why upgrades stay manual: plugin.md, "Upgrades are manual".
//
// It is the entry point plugin.md's [execution] block names, and run.sh beside
// it is the manual shim. A report exits 0, a missing brew or an unreadable
// probe exits 1, so a broken brew never prints the false all-clear (gt-th5it).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	pluginName = "tool-updater"

	// brewEnv stops brew's own auto-update on both calls: the refresh below is
	// the only index update this program runs, so a stale index stays visible
	// instead of being fixed behind the report's back (gt-th5it).
	brewEnv = "HOMEBREW_NO_AUTO_UPDATE=1"

	// updateTailLines is how much of `brew update` reaches the log: the end is
	// what explains a failed refresh and the middle is progress noise.
	updateTailLines = 3
)

// tools are reported on in this order, and actionableTool is the one an
// operator acts on by hand. Why beads is reported but not acted on: plugin.md,
// "Upgrades are manual".
var (
	tools          = []string{"dolt", "beads"}
	actionableTool = "dolt"
)

// runner runs one command and answers with the output the caller asked for:
// both streams in the order the program wrote them, or stdout alone.
type runner func(name string, extraEnv []string, combined bool, args ...string) (string, error)

// lookup finds a program on PATH.
type lookup func(name string) (string, error)

// env is everything the report reads from outside itself, so a test drives
// every branch without a brew, a subprocess or the host's PATH.
type env struct {
	out  io.Writer
	look lookup
	cmd  runner
}

func main() {
	os.Exit(run(env{out: os.Stdout, look: exec.LookPath, cmd: execRun}))
}

// run prints the report and returns the exit code it deserves.
func run(e env) int {
	if _, err := e.look("brew"); err != nil {
		e.log("ERROR: brew not found on PATH; nothing to report")
		return 1
	}

	stale := false
	e.log("Running brew update...")
	update, err := e.cmd("brew", []string{brewEnv}, true, "update")
	for _, line := range tailLines(update) {
		fmt.Fprintln(e.out, line)
	}
	if err != nil {
		stale = true
		e.log("WARNING: brew update failed; the Homebrew formula index may be stale")
	}

	e.log("Checking for updates...")
	probe, err := e.cmd("brew", []string{brewEnv}, false, "outdated", "--json=v2")
	if err != nil {
		e.log("ERROR: brew outdated failed; cannot determine tool status")
		return 1
	}
	formulae, err := parseOutdated(probe)
	if err != nil {
		e.log("ERROR: unreadable brew outdated output; cannot determine tool status")
		return 1
	}

	outdated := 0
	for _, tool := range tools {
		f, found := find(formulae, tool)
		if !found {
			e.log("  %s: up to date", tool)
			continue
		}
		installed, available := f.versions()
		e.log("  %s: update available (installed: %s, available: %s)", tool, installed, available)
		outdated++
		if tool == actionableTool {
			e.log("    to act, run by hand: brew upgrade %s", tool)
		}
	}

	// Every tracked tool with an update counts, so the summary can never call
	// the report "all current" while one is behind; actionableTool only adds
	// the operator's next step (gt-th5it).
	summary := fmt.Sprintf("%s: all tools current", pluginName)
	if outdated > 0 {
		summary = fmt.Sprintf("%s: outdated=%d (report-only)", pluginName, outdated)
	}
	// A stale index cannot support "all tools current", so say so in the title.
	if stale {
		summary += " [formula index stale: brew update failed]"
	}
	e.log("")
	e.log("=== Done === %s", summary)
	e.log("Upgrades are deliberate and manual (gt-th5it).")

	// The gate is manual, so nothing else records this run: the receipt carries
	// the summary for plugin history and the cooldown gate. A receipt that
	// cannot be written never fails the report.
	_, _ = e.cmd("gt", nil, true, "plugin", "record-run", "--plugin", pluginName, "--result", "success", "--title", summary)
	return 0
}

// log prints one report line under the plugin's name, on stdout, which is
// where the daemon's run log and a manual run both read it.
func (e env) log(format string, args ...any) {
	fmt.Fprintf(e.out, "[%s] %s\n", pluginName, fmt.Sprintf(format, args...))
}

// formula is one entry of the probe's formulae list.
type formula struct {
	Name              string   `json:"name"`
	InstalledVersions []string `json:"installed_versions"`
	CurrentVersion    *string  `json:"current_version"`
}

// versions returns what the report prints for a tool: the installed version
// and the available one, "?" where the probe carries neither. The last
// installed version is the one in force, as `brew outdated` orders them.
func (f formula) versions() (installed, available string) {
	installed = "?"
	if n := len(f.InstalledVersions); n > 0 {
		installed = f.InstalledVersions[n-1]
	}
	available = "?"
	if f.CurrentVersion != nil && *f.CurrentVersion != "" {
		available = *f.CurrentVersion
	}
	return installed, available
}

// parseOutdated reads the formulae list out of `brew outdated --json=v2`.
// A probe whose answer is not a list of entries is an error, never an empty
// report: the all-clear is the one verdict an unreadable probe must not earn
// (gt-th5it).
//
// One entry that is not an object fails the whole parse, where run.sh's
// per-tool lookup called a method on it and read the resulting error as that
// tool being up to date. Calling a tool current on an entry nothing could read
// is the false all-clear this function refuses (gt-tpg0p.3).
func parseOutdated(probe string) ([]formula, error) {
	var report struct {
		Formulae json.RawMessage `json:"formulae"`
	}
	if err := json.Unmarshal([]byte(probe), &report); err != nil {
		return nil, fmt.Errorf("decoding the probe: %w", err)
	}
	if list := bytes.TrimSpace(report.Formulae); len(list) == 0 || list[0] != '[' {
		return nil, errors.New("the probe's formulae is not a list")
	}
	var formulae []formula
	if err := json.Unmarshal(report.Formulae, &formulae); err != nil {
		return nil, fmt.Errorf("decoding the formulae list: %w", err)
	}
	return formulae, nil
}

// find returns the first entry named tool, which is the one the report reads.
func find(formulae []formula, tool string) (formula, bool) {
	for _, f := range formulae {
		if f.Name == tool {
			return f, true
		}
	}
	return formula{}, false
}

// tailLines returns the last updateTailLines lines of s the way `tail` prints
// them: a trailing newline ends the last line rather than adding an empty one.
func tailLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > updateTailLines {
		lines = lines[len(lines)-updateTailLines:]
	}
	return lines
}

// execRun is the real runner: the program runs with this process's
// environment plus extraEnv, and combined puts both streams in one buffer so
// their order survives, as the shell's 2>&1 did.
func execRun(name string, extraEnv []string, combined bool, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: the program and its arguments are this package's own strings
	cmd.Env = append(os.Environ(), extraEnv...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if combined {
		cmd.Stderr = &buf
	}
	return outputOf(&buf, cmd.Run)
}

// outputOf runs the prepared command and answers with everything it wrote,
// plus its own error: a failing run still reports the output that explains it.
func outputOf(buf *bytes.Buffer, perform func() error) (string, error) {
	if err := perform(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}
