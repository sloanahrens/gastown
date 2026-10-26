// Package repoguard holds the guards over the repository's file layout: the
// kinds of file the tree may still add, held to allowlists that only shrink.
package repoguard

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Lister returns the repo-relative paths a guard checks. Callers inject one so
// a guard's unit tests read no working tree.
type Lister func() ([]string, error)

// CheckShellScripts holds the .sh files list returns to allowed: one finding
// for a tracked script with no allowlist line, and one for an allowlisted path
// no longer tracked, so the allowlist can only shrink. A lister error is
// returned, never read as a clean tree.
func CheckShellScripts(list Lister, allowed map[string]bool) ([]string, error) {
	tracked, err := list()
	if err != nil {
		return nil, err
	}
	var findings []string
	seen := make(map[string]bool, len(tracked))
	for _, path := range tracked {
		seen[path] = true
		if !allowed[path] {
			findings = append(findings, fmt.Sprintf("%s: no new .sh files; write Go (see ~/.claude/docs/standards/test-engineering.md, Scripts are debt)", path))
		}
	}
	for path := range allowed {
		if seen[path] {
			continue
		}
		findings = append(findings, fmt.Sprintf("%s: allowlisted .sh file no longer exists; delete its line so shell-scripts.txt only shrinks", path))
	}
	sort.Strings(findings)
	return findings, nil
}

// ReadAllowlist reads a guard allowlist: one repo-relative path per line, with
// blank lines and lines starting with # skipped.
func ReadAllowlist(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	allowed := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allowed[line] = true
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return allowed, nil
}

// TrackedShellScripts lists the .sh files git tracks under root, sorted. It
// runs git from this non-test file because a unit-tier test file that starts
// git raises the realgit.txt ratchet (docs/testing.md, "Seams for external
// tools"); a caller's test still gets a failure, not a pass, when git cannot
// run.
func TrackedShellScripts(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--", "*.sh").Output() //nolint:gosec // G204: fixed argv; root is the caller's repository
	if err != nil {
		return nil, fmt.Errorf("git ls-files '*.sh': %w", err)
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	sort.Strings(files)
	return files, nil
}
