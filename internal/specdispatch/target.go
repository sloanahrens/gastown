package specdispatch

import (
	"fmt"
	"regexp"
	"strings"
)

// constraintsField is the section the path check reads and the field its
// refusal names. A path the Goal introduces is a file the spec adds, so the
// check takes the files it checks from Constraints alone (gt-7t1im).
const constraintsField = "## Constraints"

// TargetTree is the file inventory of the branch a spec's work lands on, read
// by the caller like Children: the lint checks the repo files a bead's
// Constraints names against it (gt-7t1im).
type TargetTree struct {
	// Branch names the branch the inventory came from, for a refusal's phrase
	// ("absent from main"). Empty is reported as "the target branch".
	Branch string
	// Files are the repo-relative paths the branch holds.
	Files map[string]struct{}
	// Err is why the inventory could not be read: no repo, no such branch, a
	// git failure. A tree carrying an error is never read as an empty one, so
	// a bead naming a path is held with the error named.
	Err string
}

// targetPathRe matches a repo-relative file path: at least one directory, and a
// final segment carrying an extension. A directory, a package, a command and a
// symbol are not files to change, so none of them is checked.
var targetPathRe = regexp.MustCompile(`^(?:[A-Za-z0-9_.+-]+/)+[A-Za-z0-9_.+-]*\.[A-Za-z][A-Za-z0-9]{0,7}$`)

// targetLineRefRe is the ":<line>" (or ":<line>:<col>") a citation appends.
var targetLineRefRe = regexp.MustCompile(`:\d+(?::\d+)?$`)

// targetPathTrim is the punctuation a path wears in prose: wrapping quotes,
// backticks and brackets, and a trailing comma, semicolon or full stop.
const targetPathTrim = " \t`'\"()[]<>,;:."

// targetPathReject are the characters that make a token something other than
// one literal path: a glob, a URL fragment, a comparison, an interpolation.
const targetPathReject = "*?{}|$=+^\\\"'<>()[] \t"

// introducedPhrases are the phrases in which a spec says it adds a file. A
// path a line carries one of is a file the spec introduces, not one the branch
// is expected to hold already, so it is never refused for being absent
// (gt-7t1im).
var introducedPhrases = regexp.MustCompile(`(?i)\b(?:new file|new path|new source file|does(?: not|n't)(?: yet)? exist|will be created|is created)\b`)

// introducedVerbs are the verbs a spec puts directly before the path it adds
// ("adds internal/cli/foo.go", "create docs/guides/bar.md"). The verb is
// matched next to the path, not anywhere on the line: "adds the check to
// internal/cmd/spec.go" names a file it changes.
var introducedVerbs = `(?:add|adds|added|create|creates|created|introduce|introduces|introduced)`

// constraintFilePaths returns the repo-relative files a Constraints body names,
// in first-seen order and without duplicates.
func constraintFilePaths(constraints string) []string {
	var out []string
	seen := map[string]bool{}
	for _, token := range pathTokens(constraints) {
		path, ok := targetPathOf(token)
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// pathTokens splits a prose body into the tokens a path can be read from.
func pathTokens(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';':
			return true
		}
		return false
	})
}

// targetPathOf reads one prose token as a repo-relative file path. Only the
// literal form counts, so a glob, a URL, an absolute path or a symbol is read
// as "not a file to change" and passed rather than guessed at.
func targetPathOf(token string) (string, bool) {
	path := strings.Trim(token, targetPathTrim)
	path = targetLineRefRe.ReplaceAllString(path, "")
	path = strings.Trim(path, targetPathTrim)
	if path == "" || strings.ContainsAny(path, targetPathReject) || !targetPathRe.MatchString(path) {
		return "", false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	return path, true
}

// introducedFilePaths returns the paths the spec's own words say it adds. Both
// the Goal and the Constraints are read: a spec may introduce a file in
// either, and a path introduced in one is never a file the branch is expected
// to hold already (gt-7t1im).
func introducedFilePaths(sections map[string]string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, key := range []string{"goal", "constraints"} {
		for _, line := range strings.Split(sections[key], "\n") {
			for _, token := range pathTokens(line) {
				path, ok := targetPathOf(token)
				if !ok || !introducesPath(line, path) {
					continue
				}
				out[path] = struct{}{}
			}
		}
	}
	return out
}

// introducesPath reports whether line carries the path as a file the spec adds.
// The words are matched literally — a phrase naming new files, or an add,
// create or introduce verb standing next to this path — so a line that merely
// names the path reads as "not introduced" and the path stays checked.
func introducesPath(line, path string) bool {
	line = strings.ToLower(line)
	if introducedPhrases.MatchString(line) {
		return true
	}
	adjacent := regexp.MustCompile(`\b` + introducedVerbs + `\s+[` + "`" + `"']?(?:the\s+|a\s+|an\s+)?` +
		regexp.QuoteMeta(strings.ToLower(path)) + `\b`)
	return adjacent.MatchString(line)
}

// targetRefusal is the shape failure a Constraints body earns against the
// target branch: every file the spec changes and the branch does not hold is
// named, or the error that stopped the inventory being read is. A bead naming
// no file the branch must already hold passes. A caller that supplied no
// inventory is refused rather than passed: a check that did not run is not a
// check that passed (gt-7t1im).
func targetRefusal(sections map[string]string, target *TargetTree) (Refusal, bool) {
	paths := constraintFilePaths(sections["constraints"])
	if len(paths) == 0 {
		return Refusal{}, false
	}
	introduced := introducedFilePaths(sections)
	checked := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := introduced[path]; ok {
			continue
		}
		checked = append(checked, path)
	}
	if len(checked) == 0 {
		return Refusal{}, false
	}
	if target == nil {
		return Refusal{Field: constraintsField, Reason: fmt.Sprintf("cannot check %s: no target inventory", strings.Join(checked, ", "))}, true
	}
	if target.Err != "" {
		return Refusal{Field: constraintsField, Reason: fmt.Sprintf("cannot check %s: %s", strings.Join(checked, ", "), target.Err)}, true
	}
	var missing []string
	for _, path := range checked {
		if _, ok := target.Files[path]; !ok {
			missing = append(missing, path)
		}
	}
	if len(missing) == 0 {
		return Refusal{}, false
	}
	branch := target.Branch
	if branch == "" {
		branch = "the target branch"
	}
	return Refusal{Field: constraintsField, Reason: fmt.Sprintf("names %s, absent from %s", strings.Join(missing, ", "), branch)}, true
}
