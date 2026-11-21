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
func Plan(spec Spec, groups []FileGroup) (SplitPlan, error) {
	if len(groups) < 2 {
		return SplitPlan{}, fmt.Errorf("a split needs at least two file groups, got %d", len(groups))
	}
	if err := checkGroups(spec, groups); err != nil {
		return SplitPlan{}, err
	}
	half := (len(groups) + 1) / 2
	return SplitPlan{
		A: buildPiece("A", groups[:half], groups, nil, "", spec.BaseFiles),
		B: buildPiece("B", groups[half:], groups, groups[:half], "A", spec.BaseFiles),
	}, nil
}

// checkGroups refuses a group the pieces could not carry: no package, no box, a
// box with no command, or boxes that do not match the spec's exactly once.
func checkGroups(spec Spec, groups []FileGroup) error {
	inSpec := map[Box]bool{}
	for _, box := range boxes(spec.Acceptance) {
		inSpec[box] = true
	}
	seen := map[Box]int{}
	for i, group := range groups {
		if group.Package == "" {
			return fmt.Errorf("file group %d names no package", i+1)
		}
		if len(group.Boxes) == 0 {
			return fmt.Errorf("file group %s has no acceptance boxes, so its piece would have no tests", group.Package)
		}
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
			seen[box]++
			if seen[box] > 1 {
				return fmt.Errorf("the box %q is held by more than one file group", box.Text)
			}
		}
	}
	for _, box := range boxes(spec.Acceptance) {
		if seen[box] == 0 {
			return fmt.Errorf("the file groups do not hold the spec box %q", box.Text)
		}
	}
	return nil
}

// buildPiece shapes one piece from its groups, reading allGroups for the guard
// commands and createdFrom for the files and symbols the piece depends on. A file
// in baseFiles exists on the base branch, so no piece creates it.
func buildPiece(name string, groups, allGroups, createdFrom []FileGroup, dependsOn string, baseFiles map[string]bool) Piece {
	var packages []string
	var ownBoxes []Box
	var constraints []string
	for _, group := range groups {
		packages = append(packages, group.Package)
		ownBoxes = append(ownBoxes, group.Boxes...)
		constraints = append(constraints, group.Constraints...)
	}
	if dependsOn != "" {
		constraints = append(constraints, createdNotes(createdFrom, groups, baseFiles)...)
	}
	piece := Piece{
		Name:      name,
		Packages:  packages,
		Boxes:     ownBoxes,
		DependsOn: dependsOn,
	}
	piece.Text = pieceText(piece, constraints, pieceGate(ownBoxes, allGroups), otherPackages(packages, allGroups))
	return piece
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
func pieceText(piece Piece, constraints []string, gate string, other []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Goal\n\n%s\n\n", pieceGoal(piece))
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

// pieceGoal is the Goal sentence naming the piece's file groups.
func pieceGoal(piece Piece) string {
	return fmt.Sprintf("Deliver piece %s: the %s of the split bead, with its Constraints bullets and acceptance boxes.",
		piece.Name, groupPhrase(piece.Packages))
}

// pieceOutOfScope names the other piece and everything outside this one.
func pieceOutOfScope(piece Piece, other []string) string {
	name := "B"
	if piece.Name == "B" {
		name = "A"
	}
	return fmt.Sprintf("Piece %s (%s) and anything outside piece %s's %s.",
		name, strings.Join(other, ", "), piece.Name, groupNoun(len(piece.Packages)))
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
// bGroups bullet cites. A file that exists on the base branch (baseFiles) gets
// no note: a created-by note on an existing path is the spec lint's trap.
func createdNotes(aGroups, bGroups []FileGroup, baseFiles map[string]bool) []string {
	aPackages := map[string]bool{}
	var aBullets []string
	for _, group := range aGroups {
		aPackages[group.Package] = true
		aBullets = append(aBullets, group.Constraints...)
	}
	var bBullets []string
	for _, group := range bGroups {
		bBullets = append(bBullets, group.Constraints...)
	}
	var notes []string
	for _, bullet := range aBullets {
		for _, name := range createdNames(bullet, aPackages) {
			if !name.symbol && baseFiles[name.text] {
				continue
			}
			cited := citesPath(bBullets, name.text)
			if name.symbol {
				cited = citesSymbol(bBullets, name.text)
			}
			if cited {
				notes = append(notes, name.note())
			}
		}
	}
	return unique(notes)
}

// createdName is a file or symbol a bullet names.
type createdName struct {
	// text is the path or symbol as written.
	text string
	// symbol marks a backticked identifier rather than a repo path.
	symbol bool
}

// note is the Constraints sentence that records which piece created the name.
func (n createdName) note() string {
	if n.symbol {
		return "`" + n.text + "` is created by bead PIECE_A."
	}
	return n.text + " is created by bead PIECE_A."
}

var (
	// backtickSpanRe matches one span of backticked code.
	backtickSpanRe = regexp.MustCompile("`([^`]*)`")
	// symbolSpanRe matches a code span that names a symbol: an identifier that
	// is called or stands alone, not one followed by a selector.
	symbolSpanRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(|$)`)
)

// createdNames returns the files, in aPackages, and the symbols one bullet
// names.
func createdNames(bullet string, aPackages map[string]bool) []createdName {
	var names []createdName
	for _, token := range strings.FieldsFunc(bullet, splitToken) {
		path, ok := pathToken(token)
		if !ok || !isFilePath(path) || !aPackages[parentDir(path)] {
			continue
		}
		names = append(names, createdName{text: path})
	}
	for _, span := range backtickSpanRe.FindAllStringSubmatch(bullet, -1) {
		if m := symbolSpanRe.FindStringSubmatch(strings.TrimSpace(span[1])); m != nil {
			names = append(names, createdName{text: m[1], symbol: true})
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

// citesSymbol reports whether any of the bullets cites the symbol by name.
func citesSymbol(bullets []string, symbol string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(symbol) + `\b`)
	for _, bullet := range bullets {
		if re.MatchString(bullet) {
			return true
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
