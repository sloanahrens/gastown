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

// The cues below sit directly beside a path occurrence, in the same line. A
// path is only read against the cue next to it, never a cue elsewhere on the
// line, so one spec phrase cannot exempt a path it does not describe (gt-7t1im).
//
// introducedBefore and introducedAfter mark a path the spec adds: "a new file
// X", "adds X", "X does not exist yet", "X will be created".
var (
	introducedBefore = regexp.MustCompile("(?i)\\b(?:new (?:source )?file|new path|add|adds|added|create|creates|created|introduce|introduces|introduced)\\s+[`\"']?(?:the\\s+|an?\\s+)?$")
	introducedAfter  = regexp.MustCompile("(?i)^[`\"']?\\s+(?:does(?: not|n't)(?: yet)? exist|will be created|is created)\\b")
)

// referenceBefore and referenceAfter mark a path the spec only reads, not one it
// changes: "see X", "per X", "as in X", "X is read-only". A path is passed as a
// reference only when every occurrence of it in Constraints carries one of these
// cues, so a path the spec both cites and changes is still checked.
var (
	referenceBefore = regexp.MustCompile("(?i)\\b(?:see|per|read|refer to|references?|matches|mirrors|follows|as in|like)\\s+[`\"']?(?:the\\s+|an?\\s+)?$")
	referenceAfter  = regexp.MustCompile("(?i)^[`\"']?\\s+(?:is read-only|is unchanged|is the reference)\\b")
)

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

// cuedOccurrences counts the occurrences of path in text and how many of them
// a cue marks: before matches the words ending just before the path, after the
// words starting just after it. Each line is read on its own, so a cue never
// reaches across a line break.
func cuedOccurrences(text, path string, before, after *regexp.Regexp) (cued, total int) {
	for _, line := range strings.Split(text, "\n") {
		for off := 0; ; {
			i := strings.Index(line[off:], path)
			if i < 0 {
				break
			}
			at := off + i
			end := at + len(path)
			total++
			if before.MatchString(line[:at]) || after.MatchString(line[end:]) {
				cued++
			}
			off = end
		}
	}
	return cued, total
}

// introducedBy reports whether the spec's Goal or Constraints says it adds path.
// One introducing occurrence is enough: the spec states the file is new.
func introducedBy(sections map[string]string, path string) bool {
	for _, key := range []string{"goal", "constraints"} {
		if cued, _ := cuedOccurrences(sections[key], path, introducedBefore, introducedAfter); cued > 0 {
			return true
		}
	}
	return false
}

// referenceOnly reports whether every mention of path in Constraints is a
// reference cue, so the spec reads the file and does not change it.
func referenceOnly(constraints, path string) bool {
	cued, total := cuedOccurrences(constraints, path, referenceBefore, referenceAfter)
	return total > 0 && cued == total
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
	checked := make([]string, 0, len(paths))
	for _, path := range paths {
		if introducedBy(sections, path) || referenceOnly(sections["constraints"], path) {
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
