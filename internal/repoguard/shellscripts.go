// Package repoguard holds the guards over the repository's file layout: the
// kinds of file the tree may still add, held to allowlists that only shrink.
package repoguard

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// shellScriptsAllowlist is the allowlist ParseAllowlist reads, named in every
// finding it returns so a malformed line is one grep away.
const shellScriptsAllowlist = "shell-scripts.txt"

// Lister returns the repo-relative paths a guard checks. Callers inject one so
// a guard's unit tests read no working tree.
type Lister func() ([]string, error)

// Allowlist is a shell-scripts.txt read: the paths a line with an acceptable
// reason admits, and one finding per line the guard rejects. A rejected line
// admits nothing, so a bad reason cannot stand in for a good one.
type Allowlist struct {
	Paths    map[string]bool
	Findings []string
}

// CheckShellScripts holds the .sh files list returns to the paths allowed
// admits: one finding for a listed script with no allowlist line, and one for
// an allowlist line whose script list no longer returns. The findings
// ParseAllowlist reported for malformed lines are carried through, so the tree
// guard fails on them too. A lister error is returned, never read as a clean
// tree.
func CheckShellScripts(list Lister, allowed Allowlist) ([]string, error) {
	tracked, err := list()
	if err != nil {
		return nil, err
	}
	findings := append([]string(nil), allowed.Findings...)
	seen := make(map[string]bool, len(tracked))
	for _, path := range tracked {
		seen[path] = true
		if !allowed.Paths[path] {
			findings = append(findings, fmt.Sprintf("%s: no new .sh files; write Go (scripts are debt: see .om.json, scripts-are-debt)", path))
		}
	}
	for _, path := range sortedPaths(allowed.Paths) {
		if seen[path] {
			continue
		}
		findings = append(findings, fmt.Sprintf("%s: allowlisted .sh file is no longer tracked; the shell-scripts.txt line is stale", path))
	}
	sort.Strings(findings)
	return findings, nil
}

// ReadAllowlist opens a guard allowlist and parses it; see ParseAllowlist.
func ReadAllowlist(path string) (Allowlist, error) {
	f, err := os.Open(path)
	if err != nil {
		return Allowlist{}, err
	}
	defer f.Close()
	return ParseAllowlist(f)
}

// ParseAllowlist reads the shell allowlist: one "path<TAB>reason" line per
// tracked .sh file, with blank lines and lines starting with # skipped. A line
// that carries no reason the guard accepts admits nothing and yields a
// finding, so a line that lost its reason in a merge fails the guard instead
// of reading as fine (gt-tpg0p.15).
//
// The reasons are closed:
//
//   - "platform: container entrypoint" admits docker-entrypoint.sh alone;
//   - "platform: git hook" admits an extensionless file directly in .githooks,
//     which git runs by name and never a .sh;
//   - "port: <bead id>" names the bead that deletes the line.
//
// Each platform reason is bound to its path here, in Go, so letting a new file
// stay without a port is a change a reviewer sees rather than a free-text line
// (gt-tpg0p.15). The port id is judged by shape only: the guard runs with no
// bead database, so whether the id exists is the landing's business, not this
// parser's.
//
// A duplicate path is a finding, not a silent overwrite: the file holds one
// line per script, and a second line is a merge artifact.
func ParseAllowlist(r io.Reader) (Allowlist, error) {
	allowed := Allowlist{Paths: map[string]bool{}}
	firstLine := map[string]int{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		path, reason, hasTab := strings.Cut(text, "\t")
		path = strings.TrimSpace(path)
		switch {
		case path == "":
			allowed.Findings = append(allowed.Findings, fmt.Sprintf("%s:%d: the line names no path before its tab", shellScriptsAllowlist, line))
			continue
		case !hasTab:
			// Path and reason joined by spaces is not a missing reason, and
			// naming it one would send the reader looking for the wrong fix.
			allowed.Findings = append(allowed.Findings, fmt.Sprintf("%s:%d: %q has no tab; write \"path<TAB>reason\"", shellScriptsAllowlist, line, trimmed))
			continue
		}
		if first, dup := firstLine[path]; dup {
			allowed.Findings = append(allowed.Findings, fmt.Sprintf("%s:%d: %s already has a line at line %d", shellScriptsAllowlist, line, path, first))
			continue
		}
		firstLine[path] = line
		if problem := reasonProblem(path, strings.TrimSpace(reason)); problem != "" {
			allowed.Findings = append(allowed.Findings, fmt.Sprintf("%s:%d: %s: %s", shellScriptsAllowlist, line, path, problem))
			continue
		}
		allowed.Paths[path] = true
	}
	if err := sc.Err(); err != nil {
		return Allowlist{}, err
	}
	return allowed, nil
}

// The two reasons a file may stay without a porting bead (gt-tpg0p decision
// (d)): the container runtime names one entrypoint, and git runs hooks by
// name from .githooks.
const (
	containerEntrypointReason = "platform: container entrypoint"
	gitHookReason             = "platform: git hook"
)

// portReason is the prefix of a reason that names the bead deleting the line.
const portReason = "port:"

// platformReasonFits reports whether a platform reason may exempt path. The
// binding is the point: a reason is a claim about one path, so exempting a new
// file needs a change here, not a line copied into the allowlist.
func platformReasonFits(reason, path string) bool {
	switch reason {
	case containerEntrypointReason:
		return path == "docker-entrypoint.sh"
	case gitHookReason:
		name, ok := strings.CutPrefix(path, ".githooks/")
		return ok && name != "" && !strings.Contains(name, "/") && !strings.HasSuffix(name, ".sh")
	}
	return false
}

// beadID matches the ids the bead store issues: a lettered prefix, a hyphen,
// then a lower- or upper-case body with hyphens, and any dotted child
// segments.
var beadID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(-[A-Za-z0-9]+)+(\.[A-Za-z0-9]+)*$`)

// reasonProblem returns why reason may not justify path, or "" when it may.
func reasonProblem(path, reason string) string {
	if reason == "" {
		return `no reason; say "platform: container entrypoint", "platform: git hook" or "port: <bead id>"`
	}
	if rest, ok := strings.CutPrefix(reason, portReason); ok {
		id := strings.TrimSpace(rest)
		if !beadID.MatchString(id) {
			return fmt.Sprintf("%q is not a bead id; a port reason names the bead that deletes this line, as \"port: gt-abc12\"", id)
		}
		return ""
	}
	switch reason {
	case containerEntrypointReason, gitHookReason:
		if !platformReasonFits(reason, path) {
			return fmt.Sprintf("%q does not fit this path; each platform reason is bound to the file it may exempt", reason)
		}
		return ""
	}
	return fmt.Sprintf("%q is not a reason the guard accepts; say \"platform: container entrypoint\", \"platform: git hook\" or \"port: <bead id>\"", reason)
}
