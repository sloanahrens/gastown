package done

import "strings"

// GateRun is one Gate line that runs go test with a -run filter: the line
// trimmed of surrounding space, the package arguments go test is given, the
// pattern -run selects, and the -tags list as written. Packages, the pattern
// and the tags hold only safe characters, so the branch check can pass them
// as arguments, never through a shell (gt-hx8f5i, gt-mwzlyk). Tags is empty
// when the line names no tags or its list holds an unsafe character.
type GateRun struct {
	Line     string
	Packages []string
	Pattern  string
	Tags     string
}

// gateRunLines returns the go test lines of a bead's Gate section that narrow
// the run with -run, in the order they appear. A line that is not go test, one
// whose -run pattern or package arguments are missing or unreadable, one
// naming ./..., and one that is not a single plain command are dropped: the
// branch check reads one pattern against named packages (gt-hx8f5i).
func gateRunLines(gate string) []GateRun {
	var runs []GateRun
	for _, line := range strings.Split(gate, "\n") {
		if run, ok := gateRun(strings.TrimSpace(line)); ok {
			runs = append(runs, run)
		}
	}
	return runs
}

// shellMetacharacters end, join, redirect or rewrite a shell command, so one
// outside quotes means the Gate line is more than this single command.
const shellMetacharacters = "&|;<>()`$#!*?[]{}~\\"

// isCompound reports whether line holds a shell metacharacter outside quotes
// or leaves a quote open; either way it is not one plain command.
func isCompound(line string) bool {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '$' && endsWord(line[i+1:]):
		case strings.ContainsRune(shellMetacharacters, r):
			return true
		}
	}
	return quote != 0
}

// endsWord reports whether rest, the text after a dollar, ends the shell word
// there: the shell expands nothing after such a dollar, so it is the regexp's
// end anchor and not the start of a variable or a substitution.
func endsWord(rest string) bool {
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// packageCharacters are the runes a package argument may hold; a word with
// anything else is not passed to go test as a package.
const packageCharacters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789./_-"

// patternCharacters are the runes a -run pattern may hold. A pattern is a test
// name regexp; the set leaves out the shell-significant runes the branch check
// must never hand on.
const patternCharacters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789./|()[]{}*+?:^,-_"

// gateRun reads one Gate line, returning false for anything but the form
// "go test <package...> -run <pattern> <flags>". A word naming a package path
// is a package argument wherever it stands, so one after a leading flag is
// still read; another flag's separate argument is not told apart from it.
func gateRun(line string) (GateRun, bool) {
	if isCompound(line) {
		return GateRun{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "test" {
		return GateRun{}, false
	}
	run := GateRun{Line: line}
	for i := 2; i < len(fields); i++ {
		switch field := fields[i]; {
		case field == "-run":
			if i+1 == len(fields) {
				return GateRun{}, false
			}
			i++
			run.Pattern = unquotePattern(fields[i])
		case strings.HasPrefix(field, "-run="):
			run.Pattern = unquotePattern(strings.TrimPrefix(field, "-run="))
		case field == "-tags":
			if i+1 < len(fields) {
				i++
				run.Tags = safeTags(unquotePattern(fields[i]))
			}
		case strings.HasPrefix(field, "-tags="):
			run.Tags = safeTags(unquotePattern(strings.TrimPrefix(field, "-tags=")))
		case packageArg(field):
			run.Packages = append(run.Packages, field)
		}
	}
	if !safePattern(run.Pattern) || len(run.Packages) == 0 {
		return GateRun{}, false
	}
	for _, pkg := range run.Packages {
		if pkg == "./..." {
			return GateRun{}, false
		}
	}
	return run, true
}

// packageArg reports whether a word names a package rather than a flag or a
// flag's value: it is path-shaped and holds only package-path characters.
func packageArg(word string) bool {
	if !strings.HasPrefix(word, ".") && !strings.HasPrefix(word, "/") && !strings.Contains(word, "/") {
		return false
	}
	for _, r := range word {
		if !strings.ContainsRune(packageCharacters, r) {
			return false
		}
	}
	return true
}

// unquotePattern drops one pair of matching shell quotes around a -run
// pattern or a -tags list, so a quoted value reads as its literal text.
func unquotePattern(pattern string) string {
	if len(pattern) >= 2 {
		first, last := pattern[0], pattern[len(pattern)-1]
		if first == last && (first == '\'' || first == '"') {
			return pattern[1 : len(pattern)-1]
		}
	}
	return pattern
}

// safePattern reports whether a -run pattern is non-empty, does not start with
// a dash (the shape of a flag, meaning -run lost its value), and holds only
// pattern characters and end anchors: '^TestA$' is the commonest Gate pattern,
// and dropping it would let the line through unchecked (gt-hx8f5i).
func safePattern(pattern string) bool {
	if pattern == "" || strings.HasPrefix(pattern, "-") {
		return false
	}
	for i, r := range pattern {
		if r == '$' && anchors(pattern[i+1:]) {
			continue
		}
		if !strings.ContainsRune(patternCharacters, r) {
			return false
		}
	}
	return true
}

// tagCharacters are the runes a -tags list may hold: letters, digits, comma,
// underscore and dot.
const tagCharacters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789,_."

// safeTags returns tags as written when it holds only tag characters, and
// the empty string otherwise: a list with any other text is read as no tags,
// so no unchecked tag text is kept for go test (gt-mwzlyk).
func safeTags(tags string) string {
	for _, r := range tags {
		if !strings.ContainsRune(tagCharacters, r) {
			return ""
		}
	}
	return tags
}

// anchors reports whether a dollar followed by rest is the regexp's end
// anchor: it ends the pattern, an alternative, a group or one level of a
// subtest path. Before anything else it has the shape of a variable or a
// command substitution, which the branch check never hands on.
func anchors(rest string) bool {
	return rest == "" || strings.ContainsRune("|)/", rune(rest[0]))
}
