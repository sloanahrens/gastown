package repoguard

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// guardSuffixes are the file kinds a .repoguard-allowlist holds. Python is
// absent on purpose: it stays allowed when it is the right tool (gt-b43ra).
var guardSuffixes = []string{".sh", ".bash", ".zsh", ".pl", ".rb"}

// Entry is one .repoguard-allowlist line: a repo-relative path and the reason
// its file cannot be Go.
type Entry struct {
	Path   string
	Reason string
}

// Reader returns one allowlist's entries, from the working tree or from a
// base revision.
type Reader func() ([]Entry, error)

// ParseRepoAllowlist parses one .repoguard-allowlist document: path, a tab, a
// reason, one per line, with blank lines and # comments skipped. A line with
// no tab parses as an entry with no reason so CheckScripts names the line;
// failing the parse would report a column instead of the file an author must
// fix. The reasons are free text here, unlike shell-scripts.txt's closed set
// (ParseAllowlist), since a foreign repo explains its own scripts.
func ParseRepoAllowlist(r io.Reader) ([]Entry, error) {
	var entries []Entry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, reason, _ := strings.Cut(line, "\t")
		entries = append(entries, Entry{Path: strings.TrimSpace(path), Reason: strings.TrimSpace(reason)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// CheckScripts holds the tracked files list returns to the allowlist: every
// script file is listed with a reason, every listed path is still tracked, and
// every entry is in the base allowlist. A nil readBaseAllowlist skips the
// growth check; only --init passes nil, for a repo whose base holds no
// allowlist yet. One finding per violation, sorted by path.
func CheckScripts(list func() ([]string, error), readAllowlist, readBaseAllowlist Reader) ([]string, error) {
	tracked, err := list()
	if err != nil {
		return nil, err
	}
	entries, err := readAllowlist()
	if err != nil {
		return nil, err
	}
	var base []Entry
	if readBaseAllowlist != nil {
		if base, err = readBaseAllowlist(); err != nil {
			return nil, err
		}
	}

	// A repeated path is one entry to the checks below, and a line with no
	// reason always wins: a duplicate cannot excuse an unfinished line in
	// either order.
	listed := make(map[string]string, len(entries))
	for _, e := range entries {
		reason, ok := listed[e.Path]
		if !ok || (reason != "" && e.Reason == "") {
			listed[e.Path] = e.Reason
		}
	}
	basePaths := make(map[string]bool, len(base))
	for _, e := range base {
		basePaths[e.Path] = true
	}

	var findings []string
	trackedSet := make(map[string]bool, len(tracked))
	for _, path := range tracked {
		trackedSet[path] = true
		if !isScriptFile(path) {
			continue
		}
		if _, ok := listed[path]; !ok {
			findings = append(findings, fmt.Sprintf("%s: %s", path, alarmUnlisted))
		}
	}
	for _, path := range sortedPaths(listed) {
		if listed[path] == "" {
			findings = append(findings, fmt.Sprintf("%s: %s", path, alarmNoReason))
		}
		if !trackedSet[path] {
			findings = append(findings, fmt.Sprintf("%s: %s", path, alarmStale))
		}
		if readBaseAllowlist != nil && !basePaths[path] {
			findings = append(findings, fmt.Sprintf("%s: %s", path, alarmGrown))
		}
	}
	sort.Strings(findings)
	return findings, nil
}

const (
	alarmUnlisted = "not in .repoguard-allowlist; write it in Go, or list it with a reason if it must stay a script"
	alarmNoReason = ".repoguard-allowlist entry has no reason; each line is path<TAB>reason"
	alarmStale    = ".repoguard-allowlist entry names no tracked file; delete the line so the list only shrinks"
	alarmGrown    = ".repoguard-allowlist entry is not in the base allowlist; the list only shrinks"
)

// isScriptFile compares lower-cased so a file named SETUP.SH is judged too.
func isScriptFile(path string) bool {
	lower := strings.ToLower(path)
	for _, suffix := range guardSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}
