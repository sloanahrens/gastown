// Package storypoints estimates how long a bead should take, as a Fibonacci
// story-point value, before the bead is dispatched. This file holds the shape
// features: twelve numbers computed from the text of the bead.
package storypoints

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
)

// Seat is the class of the seat that will run the bead.
type Seat int

const (
	// SeatUnknown is the seat before one is chosen. Its is_haiku term is the
	// training mean, so it contributes nothing to the estimate.
	SeatUnknown Seat = iota
	// SeatHaiku is the Haiku seat class.
	SeatHaiku
	// SeatFlash is the Flash seat class, the other non-Haiku class; its
	// is_haiku term is 0.
	SeatFlash
)

// Shape holds the twelve shape features of one bead, in the order and under the
// names the weights file uses.
type Shape struct {
	Boxes   float64 // boxes
	Paths   float64 // paths
	Pkgs    float64 // pkgs
	DLen    float64 // dlen
	NCons   float64 // ncons
	NLab    float64 // nlab
	PodEng  float64 // podeng
	Tests   float64 // tests
	NBt     float64 // nbt
	RigGT   float64 // rig_gt
	RigDV   float64 // rig_dv
	IsHaiku float64 // is_haiku
}

var (
	pathRE = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:go|md|yaml|yml|sh|json|txt|html|py|ts|tsx|sql|mod)\b`)
	pkgRE  = regexp.MustCompile(`\b(?:internal|cmd|deploy|docs)/[A-Za-z0-9_-]+`)
	testRE = regexp.MustCompile(`\btest`)
)

// EstimateText lays a bead out in the layout the weights were fitted on: the
// text the features and the embedding are computed from. The Acceptance field
// is added only when it is not empty.
func EstimateText(issue *beads.Issue) string {
	var b strings.Builder
	b.WriteString("# " + issue.Title + " [" + issue.Status + " " + issue.Type + " P" + strconv.Itoa(issue.Priority) + "]")
	if len(issue.Labels) > 0 {
		b.WriteString(" labels: " + strings.Join(issue.Labels, ", "))
	}
	b.WriteString("\n\n## Description\n\n")
	b.WriteString(strings.TrimRightFunc(issue.Description, unicode.IsSpace) + "\n")
	if ac := strings.TrimRightFunc(issue.AcceptanceCriteria, unicode.IsSpace); ac != "" {
		b.WriteString("\n## Acceptance\n\n" + ac + "\n")
	}
	return b.String()
}

// Features computes the twelve shape features of the estimate text t for a bead
// in rig and run by seat.
func Features(t, rig string, seat Seat) Shape {
	return Shape{
		Boxes:   float64(countBoxes(t)),
		Paths:   float64(distinct(pathRE, t)),
		Pkgs:    float64(distinct(pkgRE, t)),
		DLen:    math.Log(1 + float64(utf8.RuneCountInString(t))),
		NCons:   float64(countConstraints(t)),
		NLab:    float64(countLabels(t)),
		PodEng:  boolFloat(strings.Contains(t, "pod-engine")),
		Tests:   float64(len(testRE.FindAllString(t, -1))),
		NBt:     float64(strings.Count(t, "`")),
		RigGT:   boolFloat(rig == "gastown"),
		RigDV:   boolFloat(rig == "devops"),
		IsHaiku: isHaiku(seat),
	}
}

// countBoxes counts the checkboxes after the last Acceptance heading. The
// heading is matched by prefix, as beads.AcceptanceSectionKey matches it.
func countBoxes(t string) int {
	lines := strings.Split(t, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## Acceptance") {
			start = i
		}
	}
	if start < 0 {
		return 0
	}
	n := 0
	for _, line := range lines[start+1:] {
		if strings.HasPrefix(line, "- [ ]") || strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [X]") {
			n++
		}
	}
	return n
}

// countConstraints counts the bullets in the first Constraints section, up to
// the next "## " heading.
func countConstraints(t string) int {
	lines := strings.Split(t, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(line, "## Constraints")
	})
	if start < 0 {
		return 0
	}
	n := 0
	for _, line := range lines[start+1:] {
		if strings.HasPrefix(line, "## ") {
			break
		}
		if strings.HasPrefix(line, "- ") {
			n++
		}
	}
	return n
}

// countLabels counts the comma-separated labels after "labels: " on the first
// line, the title line.
func countLabels(t string) int {
	first, _, _ := strings.Cut(t, "\n")
	_, list, found := strings.Cut(first, "labels: ")
	if !found {
		return 0
	}
	n := 0
	for _, label := range strings.Split(list, ",") {
		if strings.TrimSpace(label) != "" {
			n++
		}
	}
	return n
}

// distinct counts the distinct strings re matches in t.
func distinct(re *regexp.Regexp, t string) int {
	seen := make(map[string]struct{})
	for _, m := range re.FindAllString(t, -1) {
		seen[m] = struct{}{}
	}
	return len(seen)
}

func isHaiku(seat Seat) float64 {
	switch seat {
	case SeatHaiku:
		return 1
	case SeatFlash:
		return 0
	default:
		return isHaikuMean()
	}
}

// isHaikuMean is the training mean of is_haiku, read from the weights. Before a
// seat is chosen the term is neutralized by this mean, not by 0, which would
// read as the non-Haiku seat.
func isHaikuMean() float64 {
	mean, ok := Default().ShapeMean("is_haiku")
	if !ok {
		// Unreachable: LoadWeights accepts only the shapeFeatureNames list,
		// which includes is_haiku.
		panic("storypoints: weights have no is_haiku mean")
	}
	return mean
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
