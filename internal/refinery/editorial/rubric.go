package editorial

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// RetirementLabel is the MR-bead label that marks a deliberate rubric
// retirement: with it, an MR may remove a criterion or change its
// weight/guidance and DiffRubricAt's refusal stands aside. It is the whole
// escape hatch — an unattended path such as the batch reviewer never sets it,
// so no criterion leaves the town without a label someone chose to add.
const RetirementLabel = "rubric-retirement"

// HasRetirementLabel reports whether a label set carries RetirementLabel.
func HasRetirementLabel(labels []string) bool {
	for _, l := range labels {
		if l == RetirementLabel {
			return true
		}
	}
	return false
}

// RubricCriterion is one entry of the `rubric` array in a rig's .om.json: the
// criterion's identity, how much it moves the score, and the instruction the
// reviewer grades against.
type RubricCriterion struct {
	Name     string  `json:"name"`
	Weight   float64 `json:"weight"`
	Guidance string  `json:"guidance"`
}

// rubricDocument is the slice of a .om.json this package reads. The rubric
// array is the part of the file whose loss nothing else in the pipeline
// notices — backend, threshold, depth, timeout and context_file all fail loudly
// when they are wrong.
type rubricDocument struct {
	Rubric []RubricCriterion `json:"rubric"`
}

// ParseRubricCriteria returns the rubric array of a .om.json document; empty
// input (a deleted or truncated file) yields no criteria and no error, which
// callers read as "this side proposed no criteria at all".
func ParseRubricCriteria(data []byte) ([]RubricCriterion, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var doc rubricDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing rubric: %w", err)
	}
	return doc.Rubric, nil
}

// RubricDeltaKind names how one criterion differs between two rubrics.
type RubricDeltaKind string

const (
	// RubricRemoved means the base criterion has no entry with that name in
	// the head rubric.
	RubricRemoved RubricDeltaKind = "removed"
	// RubricWeightChanged means the criterion survives with a different
	// weight — in either direction, because weights only mean anything
	// relative to the other criteria.
	RubricWeightChanged RubricDeltaKind = "weight_changed"
	// RubricGuidanceChanged means the criterion survives with different
	// instructions, so the reviewer is told to look for something else.
	RubricGuidanceChanged RubricDeltaKind = "guidance_changed"
)

// RubricDelta is one criterion the head side of a rubric diff no longer
// carries as the base side had it.
type RubricDelta struct {
	Name string
	Kind RubricDeltaKind
	// Detail is the before/after a resubmitter needs to see, without having
	// to re-read the diff.
	Detail string
}

// DiffRubric returns one RubricDelta per base criterion that head drops,
// re-weights, or re-words. Additions are not deltas: appending a criterion
// strengthens the rubric, and refusing it would block the rubric change the
// town actively wants.
func DiffRubric(base, head []RubricCriterion) []RubricDelta {
	if len(base) == 0 {
		return nil
	}
	byName := make(map[string]RubricCriterion, len(head))
	for _, c := range head {
		byName[c.Name] = c
	}
	var deltas []RubricDelta
	for _, b := range base {
		h, ok := byName[b.Name]
		if !ok {
			deltas = append(deltas, RubricDelta{
				Name:   b.Name,
				Kind:   RubricRemoved,
				Detail: fmt.Sprintf("weight %s, gone from the rubric", formatWeight(b.Weight)),
			})
			continue
		}
		if h.Weight != b.Weight {
			deltas = append(deltas, RubricDelta{
				Name:   b.Name,
				Kind:   RubricWeightChanged,
				Detail: fmt.Sprintf("weight %s -> %s", formatWeight(b.Weight), formatWeight(h.Weight)),
			})
		}
		if normalizeGuidance(h.Guidance) != normalizeGuidance(b.Guidance) {
			deltas = append(deltas, RubricDelta{
				Name:   b.Name,
				Kind:   RubricGuidanceChanged,
				Detail: "guidance reworded",
			})
		}
	}
	return deltas
}

// DiffRubricAt returns the criteria the change from mergeBase to head removes
// from, or alters within, the rubric at rubricPath — and nil when the rubric is
// untouched or every base criterion survives unchanged.
//
// A rubric lives outside the code it reviews, and nothing else in the pipeline
// looks at it: the container gate runs the tests, docs-lint reads prose, and the
// editorial reviewer grades the diff against the DEPLOYED rubric, never against
// the one the MR proposes. So an MR that swaps a criterion out merges green
// while the town loses the guard for whatever class that criterion named
// (gt-2oi0: a criterion covering the failure-serializes-to-success class was
// replaced by an unrelated one and passed every gate). Refusing here is the
// only fail-closed answer; RetirementLabel is the deliberate way through.
//
// nil, nil means "nothing to refuse" in three cases: the rig declares no rubric,
// the diff does not touch it, or mergeBase has no readable rubric to lose. An
// unreadable rubric at head, by contrast, is a deleted rubric — every base
// criterion is reported as removed.
func DiffRubricAt(g *git.Git, repoDir, rubricPath, mergeBase, head string) ([]RubricDelta, error) {
	if rubricPath == "" {
		return nil, nil
	}
	// The diff reports repo-relative paths, so an absolute rubric path that
	// resolves under the repo is compared in that form. One that resolves
	// outside it cannot appear in any diff, so there is nothing to refuse.
	rel := rubricPath
	if filepath.IsAbs(rel) {
		r, err := filepath.Rel(repoDir, rel)
		if err != nil || strings.HasPrefix(r, "..") {
			return nil, nil
		}
		rel = r
	}
	rel = filepath.ToSlash(filepath.Clean(rel))

	touched, err := g.DiffNameOnly(mergeBase, head)
	if err != nil {
		return nil, fmt.Errorf("diff %s...%s: %w", mergeBase, head, err)
	}
	if !containsPath(touched, rel) {
		return nil, nil
	}

	baseData, err := g.ShowFile(mergeBase, rel)
	if err != nil {
		return nil, nil
	}
	base, err := ParseRubricCriteria([]byte(baseData))
	if err != nil {
		return nil, fmt.Errorf("base rubric %s at %s: %w", rel, mergeBase, err)
	}
	if len(base) == 0 {
		return nil, nil
	}

	// A head rubric that will not read back is an empty one: ShowFile fails
	// for a path the MR deleted, and ParseRubricCriteria accepts "" as no
	// criteria. Both readings report every base criterion as removed.
	headData, _ := g.ShowFile(head, rel)
	headCriteria, err := ParseRubricCriteria([]byte(headData))
	if err != nil {
		return nil, fmt.Errorf("head rubric %s at %s: %w", rel, head, err)
	}
	return DiffRubric(base, headCriteria), nil
}

// FormatRubricRegression renders the refusal a resubmitter reads: every delta
// found, why the guard exists, and the one documented way past it.
func FormatRubricRegression(deltas []RubricDelta) string {
	var b strings.Builder
	b.WriteString("this MR no longer carries every existing rubric criterion unchanged:\n")
	for _, d := range deltas {
		fmt.Fprintf(&b, "  - %s: %s (%s)\n", d.Name, d.Kind, d.Detail)
	}
	b.WriteString("A criterion is the town's guard for the class of finding it names; neither the container gate nor docs-lint reads .om.json, so a deletion is refused here instead of merging unnoticed.\n")
	fmt.Fprintf(&b, "Restore the criterion (an addition belongs beside it, not in place of it), or, when retiring it is deliberate, mark the MR before resubmitting:\n  bd update <mr-id> --add-label %s\n", RetirementLabel)
	return b.String()
}

// normalizeGuidance collapses a guidance string's whitespace so a re-wrapped
// paragraph is not reported as a change — only the words count.
func normalizeGuidance(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// formatWeight renders a criterion weight without a trailing ".0", which is how
// the integer weights in om's rubric schema are written in the file.
func formatWeight(w float64) string {
	return strconv.FormatFloat(w, 'g', -1, 64)
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
