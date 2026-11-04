package specdispatch

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	// constraintsField is the section the path check reads when a bead has no
	// Files section, and the field its refusal names. A path the Goal
	// introduces is a file the spec adds, so the check takes the files it
	// checks from Constraints alone (gt-7t1im).
	constraintsField = "## Constraints"
	// filesField is the optional section a bead may use to declare what it does
	// with each repo file. When it is present it is the path check's only
	// source, and a refusal it earns names it (gt-hgt9j.10).
	filesField = "## Files"
)

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
// backticks and brackets, and a trailing comma, semicolon or colon. A full stop
// is trimmed from the end only (targetPathTail): at the start it is part of the
// name, as in .github/workflows/ci.yml.
const targetPathTrim = " \t`'\"()[]<>,;:"

// targetPathTail is targetPathTrim plus the sentence's full stop.
const targetPathTail = targetPathTrim + "."

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
	path := strings.TrimRight(strings.TrimLeft(token, targetPathTrim), targetPathTail)
	path = targetLineRefRe.ReplaceAllString(path, "")
	path = strings.TrimRight(path, targetPathTail)
	// ./internal/x.go names the same repo-relative file as internal/x.go.
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
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

// addsRewrites and readsRewrites are the rewrites a missing-path refusal prints,
// as format strings with the path: the forms the cue grammar above reads for a
// path the bead adds, and for one it only reads. The rewrite test lints each
// form, so a form the grammar stops reading fails a test.
var (
	addsRewrites  = []string{"create %s", "%s is created by bead <id>"}
	readsRewrites = []string{"see %s"}
)

// rewriteList renders each form with path, in backticks, joined by "or".
func rewriteList(forms []string, path string) string {
	quoted := make([]string, len(forms))
	for i, form := range forms {
		quoted[i] = "`" + fmt.Sprintf(form, path) + "`"
	}
	return strings.Join(quoted, " or ")
}

// targetFixHint is the sentence a missing-path refusal appends: the cues that
// pass a path the target branch need not hold, written with the first missing
// path so the filer can copy the rewrite, and where the cue must sit. With
// several paths missing the hint is an example, since the same two rewrites
// fix each of them (gt-hgt9j.1).
func targetFixHint(path string, several bool) string {
	fix := fmt.Sprintf("write %s when the bead adds the file, or %s when it only reads it; the cue must sit directly beside the path.", rewriteList(addsRewrites, path), rewriteList(readsRewrites, path))
	if several {
		return "To fix each, for example, " + fix
	}
	return "To fix, " + fix
}

// targetRefusal is the shape failure a bead earns against the target branch.
// A bead with a Files section is checked from that section alone, whatever its
// Constraints say: every modify or read path must exist on the branch or be
// created by an open blocker, a create path need not, and a section that does
// not parse is refused with the parse error (gt-hgt9j.10, gt-hgt9j.11). A bead
// without the section keeps the cue grammar its Constraints body is read with
// (gt-7t1im); the blocker set is not read for it. A caller that supplied no
// inventory is refused rather than passed: a check that did not run is not a
// check that passed (gt-7t1im, gt-hgt9j.1).
func targetRefusal(description string, sections map[string]string, target *TargetTree, blockerCreates []string) (Refusal, bool) {
	block, present, err := ParseFiles(description)
	if present {
		if err != nil {
			return Refusal{Field: filesField, Reason: err.Error()}, true
		}
		return filesTargetRefusal(block, target, blockerCreates)
	}
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
	return Refusal{Field: constraintsField, Reason: fmt.Sprintf("names %s, absent from %s. %s", strings.Join(missing, ", "), branch, targetFixHint(missing[0], len(missing) > 1))}, true
}

// filesTargetRefusal checks a bead's Files entries against the target branch:
// a modify or read path the branch does not hold is refused with its verb and
// path named, unless an open blocker declares it creates that path; an
// inventory that could not be read refuses the paths no blocker resolves. A
// create path is the bead's own addition and needs nothing on the branch.
// Paths are compared after path.Clean on both the entry and the blocker set,
// so "./a/../b.go" and "b.go" are one path (gt-hgt9j.11).
func filesTargetRefusal(block FilesBlock, target *TargetTree, blockerCreates []string) (Refusal, bool) {
	creates := cleanedPaths(blockerCreates)
	var checked []string
	var residual []FileEntry
	for _, e := range block.Entries {
		if e.Verb != FileModify && e.Verb != FileRead {
			continue
		}
		p := path.Clean(e.Path)
		if _, ok := creates[p]; ok {
			continue
		}
		residual = append(residual, FileEntry{Verb: e.Verb, Path: p, Line: e.Line})
		if named := string(e.Verb) + ": " + p; !slices.Contains(checked, named) {
			checked = append(checked, named)
		}
	}
	if len(checked) == 0 {
		return Refusal{}, false
	}
	if target == nil {
		return Refusal{Field: filesField, Reason: fmt.Sprintf("cannot check %s: no target inventory", strings.Join(checked, ", "))}, true
	}
	if target.Err != "" {
		return Refusal{Field: filesField, Reason: fmt.Sprintf("cannot check %s: %s", strings.Join(checked, ", "), target.Err)}, true
	}
	var missing []string
	for _, e := range residual {
		if _, ok := target.Files[e.Path]; ok {
			continue
		}
		if named := string(e.Verb) + ": " + e.Path; !slices.Contains(missing, named) {
			missing = append(missing, named)
		}
	}
	if len(missing) == 0 {
		return Refusal{}, false
	}
	branch := target.Branch
	if branch == "" {
		branch = "the target branch"
	}
	return Refusal{Field: filesField, Reason: fmt.Sprintf("names %s, absent from %s and no open blocker creates it", strings.Join(missing, ", "), branch)}, true
}

// cleanedPaths is a lookup set of repo paths, each path.Clean'ed so a relative
// spelling and its clean form are one path. An entry naming no file (empty, or
// something that cleans to ".") is dropped.
func cleanedPaths(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if c := path.Clean(p); c != "." {
			set[c] = struct{}{}
		}
	}
	return set
}
