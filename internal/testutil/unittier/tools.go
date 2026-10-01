package unittier

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// refusedTools are the external tools a unit test may not start: the ones
// production code runs by name (exec.Command("<name>", ...)), and the shells
// and process tools a test would reach for. git is not here: gitfree.txt and
// realgit.txt govern it (testutil.WithoutGit). A tool started by absolute
// path, or the test binary re-executing itself, is not seen.
var refusedTools = []string{
	"bash", "bd", "brew", "claude", "cp", "curl", "diskutil", "docker", "dolt",
	"find", "gh", "go", "gofmt", "gt", "kill", "launchctl", "ls", "lsof",
	"pgrep", "pkill", "ps", "sh", "sleep", "ss", "sysctl", "systemctl", "tail",
	"tmux", "vm_stat", "which", "whoami", "xcrun", "zsh",
}

// refusedLog is the file in the tool directory where the refusing tools
// record each start.
const refusedLog = "refused.log"

// installRefusingTools writes a refusing stand-in for each refusedTools entry
// on PATH and not in allowed into a new directory, puts it first on PATH, and returns
// the directory, or "" where no stand-in is installed (Windows). A stand-in
// appends its working directory and argv to refusedLog, says why on stderr
// and exits 1.
func installRefusingTools(allowed map[string]bool) (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "gt-unittier-tools-")
	if err != nil {
		return "", fmt.Errorf("creating the refusing tools' directory: %w", err)
	}
	if err := writeRefusingTools(dir, allowed, exec.LookPath); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil { //testpolicy:allow prod-no-setenv — TestMain puts the refusing tools first on the test process's PATH before m.Run
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("putting the refusing tools on PATH: %w", err)
	}
	return dir, nil
}

// writeRefusingTools writes the refusing script into dir and links each
// refused tool's name to it. A tool lookPath does not find gets no stand-in,
// so code that looks a tool up before running it still sees it missing.
func writeRefusingTools(dir string, allowed map[string]bool, lookPath func(string) (string, error)) error {
	log := filepath.Join(dir, refusedLog)
	script := filepath.Join(dir, ".refuse")
	body := "#!/bin/sh\n" +
		"t=${0##*/}\n" +
		"printf '%s\\t%s %s\\n' \"$PWD\" \"$t\" \"$*\" >> " + shellQuote(log) + "\n" +
		"echo \"$t: the unit tier starts no $t (docs/testing.md, \\\"The rules\\\"); answer it through a seam or canned output, or move the test to the integration tier\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: the refusing tools must be executable
		return fmt.Errorf("writing the refusing tool: %w", err)
	}
	for _, name := range refusedTools {
		if allowed[name] {
			continue
		}
		if _, err := lookPath(name); err != nil {
			continue
		}
		if err := os.Symlink(script, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("linking the refusing %s: %w", name, err)
		}
	}
	return nil
}

// shellQuote quotes s as one single-quoted sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// reportRefused reports every start the refusing tools in dir recorded to w
// and returns code, forced to 1 from 0 when there was any. A missing log
// means nothing was started; an unreadable one fails the run, since it cannot
// show that.
func reportRefused(code int, dir string, w io.Writer) int {
	if dir == "" {
		return code
	}
	data, err := os.ReadFile(filepath.Join(dir, refusedLog))
	if errors.Is(err, os.ErrNotExist) {
		return code
	}
	rule := strings.Repeat("=", 72)
	if err != nil {
		fmt.Fprintf(w, "\n%s\nSUBPROCESS TRIPWIRE: cannot read the refusing tools' log: %v\n%s\n", rule, err, rule)
		return 1
	}
	if len(data) == 0 {
		return code
	}
	calls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	sort.Strings(calls)
	fmt.Fprintf(w, "\n%s\nSUBPROCESS TRIPWIRE: the unit tier started an external tool %d time(s)\n", rule, len(calls))
	for _, c := range calls {
		fmt.Fprintf(w, "  - %s\n", c)
	}
	fmt.Fprintf(w, "Each start was refused, and the refusal fails the run even when the code\n")
	fmt.Fprintf(w, "under test tolerated the error. Answer the tool through a seam or canned\n")
	fmt.Fprintf(w, "output, or move the test to the integration tier (docs/testing.md, \"The rules\").\n%s\n", rule)
	if code == 0 {
		code = 1
	}
	return code
}
