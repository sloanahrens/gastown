// Package specsplit plans a split of one work bead into two pieces, each
// carrying the Constraints bullets and acceptance boxes of one package. It is
// pure: the caller reads the bead and the module's package directories, and
// nothing here reads a bead, a repository or the filesystem.
package specsplit

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// Spec is the slice of a work bead the split planner reads.
type Spec struct {
	// Constraints is the bead's Constraints section body, bullets included.
	Constraints string
	// Acceptance is the bead's acceptance text: one boxed item per line, each
	// naming the Run: command that verifies it.
	Acceptance string
	// Dirs are the module's package directories as repo-relative slash paths,
	// such as "internal/config" and "cmd/gt", with "." for the root package.
	// They are the only authority on what a token may name as a package.
	Dirs []string
	// BaseFiles are the repo-relative slash paths of the files that exist on the
	// base branch, supplied by the caller: Plan reads no file system. A file in
	// it is never "created by" a piece, whoever edits it.
	BaseFiles map[string]bool
}

// Box is one acceptance box.
type Box struct {
	// Text is the box's text after its checkbox, its Run: sentence included,
	// so a piece can print the box unchanged.
	Text string
	// Run is the command its Run: sentence names, "" when it names none.
	Run string
}

// FileGroup is one package's share of a spec: the Constraints bullets that
// name a path in its directory and the acceptance boxes that test it.
type FileGroup struct {
	// Package is the repo-relative package directory, such as
	// "internal/config".
	Package string
	// Constraints are the bullets that name the package, in spec order.
	Constraints []string
	// Boxes are the acceptance boxes that test the package, in spec order.
	Boxes []Box
}

// items is the group's size for ordering: every bullet and every box counts
// once.
func (g FileGroup) items() int { return len(g.Constraints) + len(g.Boxes) }

// Group places a spec's Constraints bullets and acceptance boxes into the file
// groups they name, largest group first. A bullet belongs to the package
// directory of the first token that names one of the spec's Dirs, and a box to
// the package its go test Run: command tests. A bullet or box that names no
// such package is an error naming it, because a bead the planner cannot place
// cannot be split by file group. A spec whose items fall into one group returns
// that group with an error saying there is nothing to split.
func Group(spec Spec) ([]FileGroup, error) {
	dirs, err := newDirIndex(spec.Dirs)
	if err != nil {
		return nil, err
	}
	var groups []FileGroup
	index := map[string]int{}
	group := func(pkg string) *FileGroup {
		if i, ok := index[pkg]; ok {
			return &groups[i]
		}
		index[pkg] = len(groups)
		groups = append(groups, FileGroup{Package: pkg})
		return &groups[len(groups)-1]
	}

	for _, bullet := range bullets(spec.Constraints) {
		pkg, reason := dirs.bulletPackage(bullet)
		if reason != "" {
			return nil, fmt.Errorf("constraints bullet %q %s", bullet, reason)
		}
		g := group(pkg)
		g.Constraints = append(g.Constraints, bullet)
	}
	for _, box := range boxes(spec.Acceptance) {
		pkg, reason := dirs.boxPackage(box.Run)
		if reason != "" {
			return nil, fmt.Errorf("acceptance box %q %s", box.Text, reason)
		}
		g := group(pkg)
		g.Boxes = append(g.Boxes, box)
	}

	// The package breaks a tie so one spec always yields the same order.
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].items() != groups[j].items() {
			return groups[i].items() > groups[j].items()
		}
		return groups[i].Package < groups[j].Package
	})
	switch len(groups) {
	case 0:
		return nil, errors.New("spec has no Constraints bullets or acceptance boxes, so it cannot be split")
	case 1:
		return groups, fmt.Errorf("spec has only the one file group %s, so it cannot be split", groups[0].Package)
	}
	return groups, nil
}

// dirIndex answers which package directory a prose token names.
type dirIndex struct {
	// packages holds the spec's Dirs: a token's path, or the directory of the
	// file it names, must equal one exactly.
	packages map[string]bool
}

// newDirIndex builds the index, refusing a Dirs list that can place nothing.
func newDirIndex(dirs []string) (*dirIndex, error) {
	if len(dirs) == 0 {
		return nil, errors.New("spec names no package directories, so no bullet or box can be grouped")
	}
	idx := &dirIndex{packages: make(map[string]bool, len(dirs))}
	for _, dir := range dirs {
		if err := checkDir(dir); err != nil {
			return nil, err
		}
		idx.packages[dir] = true
	}
	return idx, nil
}

// checkDir refuses a directory the token reader could never match, so a
// caller's malformed Dirs entry fails here rather than silently placing
// nothing.
func checkDir(dir string) error {
	if dir == "." {
		return nil
	}
	if dir == "" || strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "~") || strings.Contains(dir, "\\") {
		return fmt.Errorf("spec package directory %q is not a repo-relative path like internal/config", dir)
	}
	for _, segment := range strings.Split(dir, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("spec package directory %q is not a clean repo-relative path like internal/config", dir)
		}
	}
	return nil
}

// placement is what a token named.
type placement int

const (
	// tokenIsProse marks a token that names no package directory the spec
	// lists, including a slash word such as "and/or" and a date such as
	// "2026/11/20".
	tokenIsProse placement = iota
	// tokenIsRoot marks the root package ".", which the caller's next bead
	// places.
	tokenIsRoot
	// tokenIsPackage marks a token that names one of the spec's Dirs.
	tokenIsPackage
)

// resolve returns the package directory a token names and what it named.
func (d *dirIndex) resolve(token string) (string, placement) {
	path, ok := pathToken(token)
	if !ok {
		return "", tokenIsProse
	}
	if path == "." {
		// The root package counts only when the spec lists it, like every
		// other package.
		if d.packages["."] {
			return "", tokenIsRoot
		}
		return "", tokenIsProse
	}
	if d.packages[path] {
		return path, tokenIsPackage
	}
	// A path whose final segment carries a dot is read as a file, so its
	// directory is the package. Only a listed directory counts, which also
	// settles the first-segment rule: every listed entry's first segment is a
	// top-level directory the spec names.
	segments := strings.Split(path, "/")
	if strings.Contains(segments[len(segments)-1], ".") {
		if dir := strings.Join(segments[:len(segments)-1], "/"); d.packages[dir] {
			return dir, tokenIsPackage
		}
	}
	return "", tokenIsProse
}

var (
	// lineRefRe matches the ":<line>" (or ":<line>:<col>") a citation appends
	// to a path.
	lineRefRe = regexp.MustCompile(`:\d+(?::\d+)?$`)
	// pathSegmentRe matches one segment of a repo-relative path.
	pathSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
)

// pathReject are the characters that make a token something other than one
// literal repo path: a glob, a URL, an interpolation, a comparison, a quoted
// name.
const pathReject = "*?{}|$=+^@\\\"'<>()[]"

// pathToken reduces one prose token to the repo path it writes, and false when
// it writes none. Only the literal form counts: a glob, an absolute path, a
// bare name and a symbol are passed over, so the caller tries the next token
// rather than guessing.
func pathToken(token string) (string, bool) {
	if rootToken(token) {
		return ".", true
	}
	path := strings.TrimLeft(token, " \t`'\"([<")
	path = strings.TrimRight(path, " \t`'\")]>,.;:")
	path = strings.TrimRight(lineRefRe.ReplaceAllString(path, ""), ".,;:")
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	path = strings.TrimSuffix(path, "/")
	if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") ||
		strings.ContainsAny(path, pathReject) || strings.Contains(path, "...") {
		return "", false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." || !pathSegmentRe.MatchString(segment) {
			return "", false
		}
	}
	return path, true
}

// splitToken separates the word tokens of prose: whitespace and the
// punctuation that ends a phrase.
var splitToken = func(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', ',', ';', '(', ')', '[', ']':
		return true
	}
	return false
}

// rootToken reports whether a token writes the root package "." rather than a
// sentence's full stop.
func rootToken(token string) bool {
	path := strings.Trim(token, " \t`'\"([<)]>,;:")
	for strings.HasPrefix(path, "./") && path != "./" {
		path = strings.TrimPrefix(path, "./")
	}
	return strings.TrimSuffix(path, "/") == "."
}

// bulletPackage returns the package directory the first token of bullet that
// names one, and a clause saying why it names none when it names none.
func (d *dirIndex) bulletPackage(bullet string) (pkg, reason string) {
	for _, token := range strings.FieldsFunc(bullet, splitToken) {
		switch p, place := d.resolve(token); place {
		case tokenIsPackage:
			return p, ""
		case tokenIsRoot:
			return "", "names the root package ., which this planner does not place"
		}
	}
	return "", "names no package the spec lists, so it cannot be grouped by package"
}

// boxPackage returns the package directory a box's Run: command tests, and a
// clause saying why it names none when it names none.
func (d *dirIndex) boxPackage(run string) (pkg, reason string) {
	targets, tree, isTest := runTargets(run)
	switch {
	case !isTest:
		return "", "names no go test command, so it cannot be grouped by package"
	case tree:
		return "", "names a package tree, which no one file group can hold"
	}
	var packages []string
	root := false
	for _, target := range targets {
		switch p, place := d.resolve(target); place {
		case tokenIsPackage:
			if !contains(packages, p) {
				packages = append(packages, p)
			}
		case tokenIsRoot:
			root = true
		}
	}
	switch {
	case root:
		return "", "names the root package ., which this planner does not place"
	case len(packages) > 1:
		return "", "names more than one package, so it cannot be grouped by package"
	case len(packages) == 1:
		return packages[0], ""
	}
	return "", "names no package the spec lists, so it cannot be grouped by package"
}

// contains reports whether list holds item.
func contains(list []string, item string) bool {
	for _, have := range list {
		if have == item {
			return true
		}
	}
	return false
}

// runTargets returns the package arguments of every go test command in a Run:
// command, in order, and whether any names a package tree. isTest is false when
// the command invokes no go test at all.
func runTargets(command string) (targets []string, tree, isTest bool) {
	for _, segment := range shellSegments(command) {
		args, ok := goTestArgs(segment)
		if !ok {
			continue
		}
		isTest = true
		for _, arg := range args {
			switch {
			case strings.HasPrefix(arg, "-"):
			case strings.Contains(arg, "..."):
				tree = true
			default:
				targets = append(targets, arg)
			}
		}
	}
	return targets, tree, isTest
}

// goTestArgs returns the arguments after "go test" in one command, and false
// when the command does not invoke go test. The binary is matched by its base
// name, so an absolute path to the toolchain counts too.
func goTestArgs(fields []string) ([]string, bool) {
	for i := 0; i+1 < len(fields); i++ {
		name := fields[i]
		if j := strings.LastIndexByte(name, '/'); j >= 0 {
			name = name[j+1:]
		}
		if name == "go" && fields[i+1] == "test" {
			return fields[i+2:], true
		}
	}
	return nil, false
}

// shellSegments splits a command into the words of each command it runs, one
// segment per shell separator, after deleting its quotes and backticks so a
// quoted or backtick-wrapped package reads like the bare form. The quotes are
// deleted rather than parsed, so an unbalanced quote cannot hide a separator.
func shellSegments(command string) [][]string {
	bare := strings.Map(func(r rune) rune {
		switch r {
		case '`', '\'', '"':
			return -1
		}
		return r
	}, command)
	var out [][]string
	for _, segment := range strings.FieldsFunc(bare, func(r rune) bool {
		switch r {
		case '&', '|', ';', '(', ')', '{', '}':
			return true
		}
		return false
	}) {
		if fields := strings.Fields(segment); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// listMarkerRe matches a list item's marker: a bullet or a number.
var listMarkerRe = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)

// bullets returns the Constraints list items, each as its text with the marker
// and any checkbox removed. beads.AcceptanceItems is the repository's one
// reader of what a list item is, so the planner, the spec lint and the close
// gates agree on which lines count.
func bullets(text string) []string {
	var out []string
	for _, item := range beads.AcceptanceItems(text) {
		bullet := item.Box
		if !item.Boxed {
			bullet = listMarkerRe.ReplaceAllString(item.Line, "")
		}
		if bullet = strings.TrimSpace(bullet); bullet != "" {
			out = append(out, bullet)
		}
	}
	return out
}

// runSentenceRe matches a box's Run: sentence and holds its command.
var runSentenceRe = regexp.MustCompile(`\bRun:\s*(.+)$`)

// boxes returns the acceptance boxed items, each with the command its Run:
// sentence names.
func boxes(text string) []Box {
	var out []Box
	for _, item := range beads.AcceptanceItems(text) {
		if !item.Boxed {
			continue
		}
		out = append(out, Box{Text: item.Box, Run: runCommand(item.Box)})
	}
	return out
}

// runCommand returns the command a box's Run: sentence names, "" when the box
// has no sentence.
func runCommand(text string) string {
	m := runSentenceRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}
