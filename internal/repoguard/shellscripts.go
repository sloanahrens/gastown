// Package repoguard holds the guards over the repository's file layout: the
// kinds of file the tree may still add, held to allowlists that only shrink.
package repoguard

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Lister returns the repo-relative paths a guard checks. Callers inject one so
// a guard's unit tests read no working tree.
type Lister func() ([]string, error)

// CheckShellScripts holds the .sh files list returns to allowed: one finding
// for a listed script with no allowlist line, and one for an allowlist line
// whose script list no longer returns. A lister error is returned, never read
// as a clean tree.
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
			findings = append(findings, fmt.Sprintf("%s: no new .sh files; write Go (scripts are debt: see .om.json, scripts-are-debt)", path))
		}
	}
	for path := range allowed {
		if seen[path] {
			continue
		}
		findings = append(findings, fmt.Sprintf("%s: allowlisted .sh file is no longer tracked; the shell-scripts.txt line is stale", path))
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
