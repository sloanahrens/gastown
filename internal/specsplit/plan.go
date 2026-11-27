package specsplit

import (
	"fmt"
	"regexp"
	"strings"
)

// Piece is one side of a split: its name, the packages it carries, the bead
// text it would carry and the piece it depends on.
type Piece struct {
	// Name is "A" or "B".
	Name string
	// Packages are the piece's file group package directories, in group order.
	Packages []string
	// Text is the full bead text the piece would carry, with the PIECE_A
	// placeholder left for the caller to replace.
	Text string
	// Boxes are the piece's acceptance boxes, in group order.
	Boxes []Box
	// DependsOn is the Name of the piece this one depends on, "" when none.
	DependsOn string
}

// SplitPlan is the two pieces of a split, A first and B depending on A.
type SplitPlan struct {
	// A takes the first half of the file groups, rounded up.
	A Piece
	// B takes the rest of the file groups and depends on A.
	B Piece
}

// Plan shapes the groups into the two pieces: A takes the first half of the
// groups rounded up, B takes the rest, and every piece's Gate is its own boxes'
// go test commands plus the guard commands the original boxes carry. A group
// that cannot become a tested piece is refused, and so are groups that do not
// hold each of the spec's boxes exactly once.
//
// spec.BaseFiles and spec.BaseSymbols must be supplied (nil is an error; an
// empty map means nothing exists yet): B says a file "is created by" A only
// when A's bullets cite it and it is absent from BaseFiles, and says a symbol
// is created by A only when A's bullets declare it in a declaration code span,
// B cites it as "<package's last segment>.<name>" ("config.Read"), and it is
// absent from BaseSymbols (keyed "internal/config.Read").
func Plan(spec Spec, groups []FileGroup) (SplitPlan, error) {
	if spec.BaseFiles == nil {
		return SplitPlan{}, fmt.Errorf("BaseFiles not supplied: the planner needs the files that exist on the base branch to say which files a piece creates; pass an empty map when none exists")
	}
	if spec.BaseSymbols == nil {
		return SplitPlan{}, fmt.Errorf("BaseSymbols not supplied: the planner needs the symbols that exist on the base branch to say which symbols a piece creates; pass an empty map when none exists")
	}
	if len(groups) < 2 {
		return SplitPlan{}, fmt.Errorf("a split needs at least two file groups, got %d", len(groups))
	}
	if err := checkGroups(spec, groups); err != nil {
		return SplitPlan{}, err
	}
	half := (len(groups) + 1) / 2
	return SplitPlan{
		A: buildPiece("A", groups[:half], groups[half:], groups, "", spec.BaseFiles, spec.BaseSymbols),
		B: buildPiece("B", groups[half:], groups[:half], groups, "A", spec.BaseFiles, spec.BaseSymbols),
	}, nil
}

// checkGroups refuses a group the pieces could not carry: no package, no box, a
// box with no command, or boxes that do not match the spec's exactly once.
func checkGroups(spec Spec, groups []FileGroup) error {
	inSpec := map[Box]bool{}
	for _, box := range boxes(spec.Acceptance) {
		inSpec[box] = true
	}
	// listed is the group's boxes, seen every group's: a repeat inside one
	// group is named for it, a box two groups hold stays a cross-group one.
	seen := map[Box]bool{}
	for i, group := range groups {
		if group.Package == "" {
			return fmt.Errorf("file group %d names no package", i+1)
		}
		if len(group.Boxes) == 0 {
			return fmt.Errorf("file group %s has no acceptance boxes, so its piece would have no tests", group.Package)
		}
		listed := map[Box]bool{}
		for _, box := range group.Boxes {
			if strings.TrimSpace(box.Run) == "" {
				return fmt.Errorf("file group %s has the box %q with no Run command", group.Package, box.Text)
			}
			if len(testCommands(box.Run)) == 0 {
				return fmt.Errorf("file group %s has the box %q whose Run names no go test command", group.Package, box.Text)
			}
			if !inSpec[box] {
				return fmt.Errorf("file group %s holds the box %q, which the spec does not", group.Package, box.Text)
			}
			if listed[box] {
				return fmt.Errorf("acceptance box %q is listed twice in group %s", box.Text, group.Package)
			}
			listed[box] = true
			if seen[box] {
				return fmt.Errorf("the box %q is held by more than one file group", box.Text)
			}
			seen[box] = true
		}
	}
	for _, box := range boxes(spec.Acceptance) {
		if !seen[box] {
			return fmt.Errorf("the file groups do not hold the spec box %q", box.Text)
		}
	}
	return nil
}

// buildPiece shapes one piece from its groups, reading other for the other
// piece's groups and allGroups for the guard commands. A file in baseFiles or a
// symbol in baseSymbols exists on the base branch, so no piece creates it.
func buildPiece(name string, groups, other, allGroups []FileGroup, dependsOn string, baseFiles, baseSymbols map[string]bool) Piece {
	var packages []string
	var ownBoxes []Box
	var constraints []string
	for _, group := range groups {
		packages = append(packages, group.Package)
		ownBoxes = append(ownBoxes, group.Boxes...)
		constraints = append(constraints, group.Constraints...)
	}
	var otherBoxes []Box
	shares := false
	for _, group := range other {
		otherBoxes = append(otherBoxes, group.Boxes...)
		shares = shares || contains(packages, group.Package)
	}
	if dependsOn != "" {
		constraints = append(constraints, createdNotes(other, groups, baseFiles, baseSymbols)...)
		if shares {
			constraints = append(constraints, sharedTestNote(other, baseFiles)...)
		}
	}
	piece := Piece{
		Name:      name,
		Packages:  packages,
		Boxes:     ownBoxes,
		DependsOn: dependsOn,
	}
	otherPiece := peer{packages: otherPackages(packages, allGroups), boxes: otherBoxes, shares: shares}
	piece.Text = pieceText(piece, constraints, pieceGate(ownBoxes, allGroups), otherPiece)
	return piece
}

// peer is the other piece as this piece's text names it.
type peer struct {
	packages []string
	boxes    []Box
	shares   bool
}

// sharedTestNote is the one Constraints bullet a piece sharing a package with the
// other piece gets: the test files the other piece's bullets cite.
func sharedTestNote(other []FileGroup, baseFiles map[string]bool) []string {
	aPackages := map[string]bool{}
	for _, group := range other {
		aPackages[group.Package] = true
	}
	var clauses []string
	for _, group := range other {
		for _, bullet := range group.Constraints {
			for _, name := range createdNames(bullet, aPackages, group.Package) {
				if name.symbol || !strings.HasSuffix(name.text, "_test.go") {
					continue
				}
				if baseFiles[name.text] {
					clauses = append(clauses, "see "+name.text)
				} else {
					clauses = append(clauses, name.text+" is created by bead PIECE_A")
				}
			}
		}
	}
	clauses = unique(clauses)
	if len(clauses) == 0 {
		return nil
	}
	return []string{strings.Join(clauses, "; ") + "."}
}

// otherPackages returns the packages the other piece carries, in group order.
func otherPackages(packages []string, allGroups []FileGroup) []string {
	own := map[string]bool{}
	for _, pkg := range packages {
		own[pkg] = true
	}
	var other []string
	for _, group := range allGroups {
		if !own[group.Package] {
			other = append(other, group.Package)
		}
	}
	return other
}

// pieceGate returns the piece's Gate: its own boxes' go test commands, then the
// guard commands every original box carries, each once in first-seen order.
func pieceGate(ownBoxes []Box, allGroups []FileGroup) string {
	var commands []string
	for _, box := range ownBoxes {
		commands = append(commands, testCommands(box.Run)...)
	}
	for _, group := range allGroups {
		for _, box := range group.Boxes {
			for _, command := range commandSegments(box.Run) {
				if !isGoTest(command) {
					commands = append(commands, command)
				}
			}
		}
	}
	return strings.Join(unique(commands), " && ")
}

// pieceText renders the piece as a bead description, the sections in the order
// a filed bead carries them.
func pieceText(piece Piece, constraints []string, gate string, other peer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Goal\n\n%s\n\n", pieceGoal(piece, other))
	b.WriteString("## Constraints\n\n")
	for _, constraint := range constraints {
		fmt.Fprintf(&b, "- %s\n", constraint)
	}
	fmt.Fprintf(&b, "\n## Out of scope\n\n%s\n\n", pieceOutOfScope(piece, other))
	fmt.Fprintf(&b, "## Gate\n\n%s\n\n", gate)
	b.WriteString("## Size\n\none worker, one landing\n\n")
	b.WriteString("## Acceptance\n\n")
	for _, box := range piece.Boxes {
		fmt.Fprintf(&b, "- [ ] %s\n", box.Text)
	}
	return b.String()
}

// pieceGoal names the piece's file groups, or its boxes when the packages are shared.
func pieceGoal(piece Piece, other peer) string {
	if other.shares {
		return fmt.Sprintf("Deliver piece %s: the split bead's %s, with its Constraints bullets.",
			piece.Name, boxPhrase(piece.Boxes))
	}
	return fmt.Sprintf("Deliver piece %s: the %s of the split bead, with its Constraints bullets and acceptance boxes.",
		piece.Name, groupPhrase(piece.Packages))
}

// pieceOutOfScope names the other piece's packages, or its deliverable when shared.
func pieceOutOfScope(piece Piece, other peer) string {
	name := "B"
	if piece.Name == "B" {
		name = "A"
	}
	if other.shares {
		return fmt.Sprintf("Piece %s's deliverable (the %s) and anything outside piece %s's %s.",
			name, boxPhrase(other.boxes), piece.Name, boxNoun(len(piece.Boxes)))
	}
	return fmt.Sprintf("Piece %s (%s) and anything outside piece %s's %s.",
		name, strings.Join(other.packages, ", "), piece.Name, groupNoun(len(piece.Packages)))
}

// boxPhrase is the boxes' texts as prose, each quoted: `the acceptance box "x"`
// for one box and `the acceptance boxes "x", "y"` for two or more.
func boxPhrase(boxes []Box) string {
	quoted := make([]string, len(boxes))
	for i, box := range boxes {
		quoted[i] = `"` + box.Text + `"`
	}
	return boxNoun(len(boxes)) + " " + strings.Join(quoted, ", ")
}

// boxNoun is "acceptance box" for one box and "acceptance boxes" otherwise.
func boxNoun(n int) string {
	if n == 1 {
		return "acceptance box"
	}
	return "acceptance boxes"
}

// groupPhrase is the packages as prose: "a, b file groups".
func groupPhrase(packages []string) string {
	return strings.Join(packages, ", ") + " " + groupNoun(len(packages))
}

// groupNoun is "file group" for one package and "file groups" otherwise.
func groupNoun(n int) string {
	if n == 1 {
		return "file group"
	}
	return "file groups"
}

// createdNotes returns a note for each file or symbol the aGroups name that a
// bGroups bullet cites. A file that exists on the base branch (baseFiles), or a
// symbol that does (baseSymbols, keyed by the declaring group's package and the
// symbol's name), gets no note: a created-by note on something that already
// exists is the spec lint's trap.
func createdNotes(aGroups, bGroups []FileGroup, baseFiles, baseSymbols map[string]bool) []string {
	aPackages := map[string]bool{}
	for _, group := range aGroups {
		aPackages[group.Package] = true
	}
	var bBullets []string
	for _, group := range bGroups {
		bBullets = append(bBullets, group.Constraints...)
	}
	var notes []string
	for _, group := range aGroups {
		for _, bullet := range group.Constraints {
			for _, name := range createdNames(bullet, aPackages, group.Package) {
				if name.symbol && baseSymbols[symbolKey(group.Package, name.text)] {
					continue
				}
				if !name.symbol && baseFiles[name.text] {
					continue
				}
				cited := citesPath(bBullets, name.text)
				if name.symbol {
					cited = citesSymbol(bBullets, name.qualified)
				}
				if cited {
					notes = append(notes, name.note())
				}
			}
		}
	}
	return unique(notes)
}

// symbolKey is BaseSymbols' key for a symbol declared in a package directory.
func symbolKey(pkg, name string) string {
	return pkg + "." + name
}

// createdName is a file or symbol a bullet names.
type createdName struct {
	// text is the path, or the symbol's bare name as A declares it.
	text string
	// qualified is the symbol as B cites it: its package's last path segment,
	// a dot and the name ("config.Read"). Empty for a file.
	qualified string
	// symbol marks a backticked identifier rather than a repo path.
	symbol bool
}

// note is the Constraints sentence that records which piece created the name.
func (n createdName) note() string {
	if n.symbol {
		return "`" + n.qualified + "` is created by bead PIECE_A."
	}
	return n.text + " is created by bead PIECE_A."
}

// packageBase is a package directory's last path segment: "config" for internal/config.
func packageBase(pkg string) string {
	return pkg[strings.LastIndexByte(pkg, '/')+1:]
}

var (
	// backtickSpanRe matches one span of backticked code.
	backtickSpanRe = regexp.MustCompile("`([^`]*)`")
	// declSpanRe matches a code span that declares a Go symbol: a func, a method,
	// a type, a const or a var, and captures the declared name.
	declSpanRe = regexp.MustCompile(`^(?:func\s+(?:\([^)]*\)\s*)?|type\s+|const\s+|var\s+)([A-Za-z_][A-Za-z0-9_]*)`)
)

// createdNames returns the files, in aPackages, and the symbols one bullet
// names, the symbols qualified by the package they are declared in.
func createdNames(bullet string, aPackages map[string]bool, pkg string) []createdName {
	var names []createdName
	for _, token := range strings.FieldsFunc(bullet, splitToken) {
		path, ok := pathToken(token)
		if !ok || !isFilePath(path) || !aPackages[parentDir(path)] {
			continue
		}
		names = append(names, createdName{text: path})
	}
	for _, span := range backtickSpanRe.FindAllStringSubmatch(bullet, -1) {
		if m := declSpanRe.FindStringSubmatch(strings.TrimSpace(span[1])); m != nil {
			names = append(names, createdName{text: m[1], qualified: packageBase(pkg) + "." + m[1], symbol: true})
		}
	}
	return names
}

// isFilePath reports whether a repo path names a file: its final segment holds
// a dot.
func isFilePath(path string) bool {
	return strings.Contains(path[strings.LastIndexByte(path, '/')+1:], ".")
}

// parentDir is the directory part of a repo path, "." at the root.
func parentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// citesPath reports whether any of the bullets cites the path.
func citesPath(bullets []string, path string) bool {
	for _, bullet := range bullets {
		for _, token := range strings.FieldsFunc(bullet, splitToken) {
			if got, ok := pathToken(token); ok && got == path {
				return true
			}
		}
	}
	return false
}

// citesSymbol reports whether any bullet cites the qualified symbol as a whole
// identifier in a code span; a bare name in B's code and the word in prose cite nothing.
func citesSymbol(bullets []string, symbol string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(symbol) + `\b`)
	for _, bullet := range bullets {
		for _, span := range backtickSpanRe.FindAllStringSubmatch(bullet, -1) {
			if re.MatchString(span[1]) {
				return true
			}
		}
	}
	return false
}

// commandSegments splits a Run command into its commands, keeping each as
// written.
func commandSegments(command string) []string {
	var out []string
	var current strings.Builder
	var quote rune
	runes := []rune(command)
	flush := func() {
		if text := strings.TrimSpace(current.String()); text != "" {
			out = append(out, text)
		}
		current.Reset()
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			current.WriteRune(r)
		case r == '\'' || r == '"' || r == '`':
			quote = r
			current.WriteRune(r)
		case r == '\n' || r == ';':
			flush()
		case r == '&' || r == '|':
			flush()
			if i+1 < len(runes) && runes[i+1] == r {
				i++
			}
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return out
}

// isGoTest reports whether a command invokes go test.
func isGoTest(command string) bool {
	for _, fields := range shellSegments(command) {
		if _, ok := goTestArgs(fields); ok {
			return true
		}
	}
	return false
}

// testCommands returns the go test commands among a Run command's segments.
func testCommands(command string) []string {
	var out []string
	for _, segment := range commandSegments(command) {
		if isGoTest(segment) {
			out = append(out, segment)
		}
	}
	return out
}

// unique returns the strings in first-seen order, each once.
func unique(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}
