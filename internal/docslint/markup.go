package docslint

import (
	"fmt"
	"regexp"
	"strings"
)

// The rules the line checks in this file report under.
const (
	RuleStatusHeader    = "status-header"
	RuleStatusConflict  = "status-conflict"
	RuleStrayMarkup     = "stray-markup"
	RulePolecatMainPush = "polecat-main-push"
)

// statusPrefix opens the status header that marks a doc's status (R12).
const statusPrefix = "> Status:"

// pushDefaultPattern is a command that sends work to main or master. The
// refspec's destination decides: "main:polecat/x" reads from main and is not a
// push to it, while "HEAD:main" and ":main" are. It is matched per line, so a
// push cannot be spliced together from two lines.
var pushDefaultPattern = regexp.MustCompile(`git[[:space:]]+(-[^[:space:]]+[[:space:]]+[^[:space:]]+[[:space:]]+)*push([[:space:]]+-[^[:space:]]+)*[[:space:]]+[^[:space:]]+[[:space:]]+([^[:space:]]*:)?(refs/heads/)?(main|master)([^[:alnum:]:_-]|$)`)

// slingPolecatPattern is an assignment to a polecat.
var slingPolecatPattern = regexp.MustCompile(`\bsling[[:space:]]+[^[:space:]]*polecats/`)

// CheckStatusHeaders reports each plan and research doc whose first non-blank
// line is not a "> Status:" header. Fences are not stripped: the header is the
// first line a reader sees, fenced or not.
func CheckStatusHeaders(fsys FS) ([]Finding, error) {
	var files []string
	for _, dir := range []string{"docs/plans", "docs/research"} {
		walked, err := fsys.WalkFiles(dir)
		if err != nil {
			return nil, err
		}
		files = append(files, walked...)
	}
	var out []Finding
	for _, f := range sortedUnique(files) {
		if !strings.HasSuffix(f, ".md") {
			continue
		}
		line, found, err := firstNonBlankLine(fsys, f)
		if err != nil {
			return nil, err
		}
		if !found || !strings.HasPrefix(line, statusPrefix) {
			out = append(out, Finding{
				File:    f,
				Line:    1,
				Rule:    RuleStatusHeader,
				Message: `first non-blank line must start with "> Status:"`,
			})
		}
	}
	return out, nil
}

// CheckStatusConflict reports each prose doc with more than one "> Status:"
// line outside a fence. One file has one status: a second line contradicts the
// first.
func CheckStatusConflict(fsys FS) ([]Finding, error) {
	files, err := linkedFiles(fsys)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range files {
		data, err := fsys.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("docs-lint: reading %s: %w", f, err)
		}
		n := 0
		for _, line := range StripFences(string(data)) {
			if strings.HasPrefix(line.Text, statusPrefix) {
				n++
			}
		}
		if n > 1 {
			out = append(out, Finding{
				File:    f,
				Line:    1,
				Rule:    RuleStatusConflict,
				Message: `more than one "> Status:" line`,
			})
		}
	}
	return out, nil
}

// CheckStrayMarkup reports each line of the doc tiers and the go tier that is
// committed tool-call or merge-conflict markup. Fences are not stripped: a
// fenced tool call is still a fragment a reader is handed.
func CheckStrayMarkup(fsys FS) ([]Finding, error) {
	files, err := markupFiles(fsys)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range files {
		data, err := fsys.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("docs-lint: reading %s: %w", f, err)
		}
		for i, text := range splitLines(string(data)) {
			if hasStrayMarkup(text) {
				out = append(out, Finding{
					File:    f,
					Line:    i + 1,
					Rule:    RuleStrayMarkup,
					Message: "committed tool-call or merge-conflict markup",
				})
			}
		}
	}
	return out, nil
}

// CheckPolecatMainPush reports each agent-facing formula that pushes the
// default branch and also offers a polecat, on each line that offers one. A
// polecat session cannot push main, so a formula that tells one to is advertising
// a flow the guards refuse (gt-ibt8). plugin.md is exempt: the plugins that push
// a default branch run from the daemon, which is not a polecat caller.
func CheckPolecatMainPush(fsys FS) ([]Finding, error) {
	agentFacing, err := AgentFacing(fsys)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range agentFacing {
		if !strings.HasSuffix(f, ".formula.toml") {
			continue
		}
		data, err := fsys.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("docs-lint: reading %s: %w", f, err)
		}
		lines := splitLines(string(data))
		if !pushesDefaultBranch(lines) {
			continue
		}
		for i, text := range lines {
			if slingPolecatPattern.MatchString(text) {
				out = append(out, Finding{
					File:    f,
					Line:    i + 1,
					Rule:    RulePolecatMainPush,
					Message: "pushes main/master and offers a polecat; the guards refuse a polecat session (gt-ibt8)",
				})
			}
		}
	}
	return out, nil
}

// markupFiles is what the stray-markup check reads: the markdown and TOML of the
// three doc tiers, and the non-test go tier.
func markupFiles(fsys FS) ([]string, error) {
	agentFacing, err := AgentFacing(fsys)
	if err != nil {
		return nil, err
	}
	reference, err := Reference(fsys)
	if err != nil {
		return nil, err
	}
	historical, err := Historical(fsys)
	if err != nil {
		return nil, err
	}
	goFiles, err := Go(fsys)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range append(append(agentFacing, reference...), historical...) {
		if strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".toml") {
			out = append(out, p)
		}
	}
	return sortedUnique(append(out, goFiles...)), nil
}

// hasStrayMarkup reports whether a line is a raw tool-call fragment or an
// unresolved merge-conflict marker. A conflict marker must be followed by a
// space, so prose that merely starts with the characters is not one.
func hasStrayMarkup(line string) bool {
	// The fragments are split so this file does not carry the text the docs-lint
	// gate scans every non-test go file for.
	for _, marker := range []string{"<" + "tool_call>", "</" + "tool_call>", "<" + "function=", "<" + "parameter="} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ")
}

// pushesDefaultBranch reports whether any line of a file pushes main or master.
func pushesDefaultBranch(lines []string) bool {
	for _, text := range lines {
		if pushDefaultPattern.MatchString(text) {
			return true
		}
	}
	return false
}
