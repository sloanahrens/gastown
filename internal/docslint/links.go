package docslint

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The rules the link and make-target checks report under.
const (
	RuleDeadLink       = "dead-link"
	RuleDeadMakeTarget = "dead-make-target"
)

// linkPattern is a markdown link: [text](target), the text free of "]".
var linkPattern = regexp.MustCompile(`\[[^\]]*\]\([^)]+\)`)

// makeRefPattern is a make invocation a reader is told to run: the backtick
// directly before "make", then a target name.
var makeRefPattern = regexp.MustCompile("`make ([A-Za-z0-9_-]+)")

// Line is one line of a file, with the number it has in that file.
type Line struct {
	No   int
	Text string
}

// Finding is one rule a file breaks. String prints it as the checks report it:
// <path>:<line>: <rule>: <message>.
type Finding struct {
	File    string
	Line    int
	Rule    string
	Message string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Rule, f.Message)
}

// StripFences returns the lines of text outside every fenced block. A line
// that starts with three backticks opens a block and the next such line closes
// it; both delimiter lines are removed, and a block never closed removes the
// rest of the text. Each remaining line keeps its number in text, so a finding
// after a fence reports the line a reader sees in the file.
func StripFences(text string) []Line {
	var out []Line
	inFence := false
	for i, line := range splitLines(text) {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			out = append(out, Line{No: i + 1, Text: line})
		}
	}
	return out
}

// CheckDeadLinks reports each relative link in the agent-facing, reference and
// historical tiers whose target does not exist. Links inside fences are not
// judged. A template renders at the repository root, so its links resolve
// from there rather than from the directory it lives in.
func CheckDeadLinks(fsys FS) ([]Finding, error) {
	files, err := linkedFiles(fsys)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range files {
		base := path.Dir(f)
		if strings.HasPrefix(f, "internal/templates/") {
			base = "."
		}
		data, err := fsys.ReadFile(f)
		if err != nil {
			// A file that cannot be read is not a file with no dead links.
			return nil, fmt.Errorf("docs-lint: reading %s: %w", f, err)
		}
		for _, line := range StripFences(string(data)) {
			for _, match := range linkPattern.FindAllString(line.Text, -1) {
				target, judged := linkTarget(match)
				if !judged {
					continue
				}
				if !fsys.PathExists(path.Join(base, target)) {
					out = append(out, Finding{
						File:    f,
						Line:    line.No,
						Rule:    RuleDeadLink,
						Message: target + " does not exist",
					})
				}
			}
		}
	}
	return out, nil
}

// CheckDeadMakeTargets reports each `make <name>` the agent-facing and reference
// tiers tell a reader to run when <name> is not a Makefile target. Fences are
// not stripped: a make line in a code block is still an instruction. With no
// Makefile there is nothing to match against, so there are no findings.
func CheckDeadMakeTargets(fsys FS) ([]Finding, error) {
	if !fsys.Exists("Makefile") {
		return nil, nil
	}
	makefile, err := fsys.ReadFile("Makefile")
	if err != nil {
		return nil, fmt.Errorf("docs-lint: reading Makefile: %w", err)
	}
	// A name is a target when a line starts with "<name>:". The name has no
	// colon, so that is exactly the text before a line's first colon.
	targets := map[string]bool{}
	for _, line := range splitLines(string(makefile)) {
		name, _, found := strings.Cut(line, ":")
		if found {
			targets[name] = true
		}
	}

	agentFacing, err := AgentFacing(fsys)
	if err != nil {
		return nil, err
	}
	reference, err := Reference(fsys)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, f := range sortedUnique(append(agentFacing, reference...)) {
		if !strings.HasSuffix(f, ".md") {
			continue
		}
		data, err := fsys.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("docs-lint: reading %s: %w", f, err)
		}
		for i, text := range splitLines(string(data)) {
			for _, m := range makeRefPattern.FindAllStringSubmatch(text, -1) {
				if !targets[m[1]] {
					out = append(out, Finding{
						File:    f,
						Line:    i + 1,
						Rule:    RuleDeadMakeTarget,
						Message: fmt.Sprintf("make %s is not a Makefile target", m[1]),
					})
				}
			}
		}
	}
	return out, nil
}

// linkedFiles is the files the link check reads: the markdown of the three
// tiers that hold prose, as one sorted list.
func linkedFiles(fsys FS) ([]string, error) {
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
	all := append(append(agentFacing, reference...), historical...)
	var out []string
	for _, p := range sortedUnique(all) {
		if strings.HasSuffix(p, ".md") {
			out = append(out, p)
		}
	}
	return out, nil
}

// linkTarget is the path a [text](target) match points at. A title after the
// path and the fragment after # are dropped. It reports false for a target the
// check does not judge: an empty one, a fragment alone, or one with a scheme
// (any "://", or mailto:).
func linkTarget(match string) (string, bool) {
	target := match[strings.Index(match, "](")+len("](") : len(match)-len(")")]
	if i := strings.IndexByte(target, ' '); i >= 0 {
		target = target[:i]
	}
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return "", false
	}
	return target, true
}

// splitLines splits text into its lines, without the empty element a final
// newline would add.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}
