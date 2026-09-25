package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/steveyegge/gastown/internal/formula"
)

// FormulaTierCheck flags a town-tier formula override that shadows the system
// tier with STALE content wearing the system tier's version number.
//
// The town tier (<town>/.beads/formulas/) wins over the embedded system tier
// on every resolve (docs/design/formula-resolution.md, tier 2 > tier 3), so a
// shadowing copy IS the formula every town agent reads. The pre-existing
// FormulaCheck and the formula version field both key on the version number:
// a copy that holds the same version as the embedded one is "up to date" in
// every check that exists — even when its bytes hold pre-fix instructions.
// That is exactly the 2026-09-19 incident (gt-aydo): mol-witness-patrol sat
// at version 18 with 30,870 bytes while main carried version 18 at 36,443
// bytes (missing the gt-xb27 Stall Judgement block), so town witnesses ran
// pre-fix instructions after the fix landed, until a human refreshed the
// copy by hand. mol-convoy-feed and mol-polecat-work hit the same hole that
// day (missing gt-yg24 and gt-pxlg text respectively).
//
// This check compares bytes for same-version overrides: identical bytes are a
// refresh (no finding); different bytes are a warning naming the file, the
// version, and both sizes. A copy whose version is ahead of the embedded one
// is a local advance and is left alone; a copy behind the embedded version is
// FormulaCheck's "outdated" story and is not duplicated here.
//
// The check is advisory and never auto-fixes: the town tier is a
// user-maintained slot, and overwriting it would discard whatever the user
// put there — refresh by hand (copy the embedded copy) or move the
// customization to a rig-tier overlay.
type FormulaTierCheck struct {
	BaseCheck
}

// NewFormulaTierCheck creates a new FormulaTierCheck.
func NewFormulaTierCheck() *FormulaTierCheck {
	return &FormulaTierCheck{
		BaseCheck{
			CheckName:        "formula-tiers",
			CheckDescription: "Flag town-tier formula overrides that are stale at the system tier's version",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run compares every town-tier formula file against the embedded system tier.
func (c *FormulaTierCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{
		Name:   c.Name(),
		Status: StatusOK,
	}

	townContent, err := loadTownTierFormulas(ctx.TownRoot)
	if err != nil {
		result.Status = StatusSkipped
		result.Message = fmt.Sprintf("Could not read town-tier formulas: %v", err)
		return result
	}

	stale := 0
	names := make([]string, 0, len(townContent))
	for filename := range townContent {
		names = append(names, filename)
	}
	sort.Strings(names) // deterministic detail order
	for _, filename := range names {
		townData := townContent[filename]
		embedded, err := formula.GetEmbeddedFormulaContent(filename)
		if err != nil {
			// The town tier may legitimately carry a formula this binary
			// no longer embeds (or one a newer binary added). Without an
			// embedded counterpart there is no version to compare against.
			continue
		}
		if string(townData) == string(embedded) {
			continue // refreshed copy, not an edit
		}

		townF, err := formula.Parse(townData)
		if err != nil {
			// A town-tier file that does not parse is the formula
			// resolver's failure to surface, not this check's: FormulaCheck
			// still reports the file as modified. Not a tier-drift finding.
			continue
		}
		embeddedF, err := formula.Parse(embedded)
		if err != nil {
			continue // embedded content is what this binary ships; trust it
		}

		switch {
		case townF.Version == embeddedF.Version:
			result.Status = StatusWarning
			stale++
			result.Details = append(result.Details, fmt.Sprintf(
				"  %s: town tier shadows the system tier at version %d with different bytes (%s vs %s) — stale until refreshed",
				filename, townF.Version,
				formatByteSize(len(embedded)), formatByteSize(len(townData))))
		case townF.Version > embeddedF.Version:
			// Local advance: a deliberately newer town-tier copy. Not
			// shadowing the system tier with an older read of it.
		default:
			// Behind the embedded version: FormulaCheck reports this as
			// "outdated" with a fix path; duplicating it here would say
			// the same thing twice.
		}
	}

	if stale > 0 {
		result.Message = fmt.Sprintf("%d town-tier formula(s) stale at the system tier's version", stale)
		result.FixHint = "Refresh the flagged copies from the embedded tier (gt formula sync --force names the hand-edited ones) or move the customization to a rig-tier overlay"
	} else {
		result.Message = "Town-tier formulas agree with the system tier"
	}

	return result
}

// loadTownTierFormulas reads every *.formula.toml in the town tier
// (<townRoot>/.beads/formulas/) and returns the filename -> bytes map. A
// missing formulas directory yields an empty map: a town with no tier-2
// overrides has nothing to check.
func loadTownTierFormulas(townRoot string) (map[string][]byte, error) {
	formulasDir := filepath.Join(townRoot, ".beads", "formulas")
	entries, err := os.ReadDir(formulasDir) //nolint:gosec // G304: path is built from the validated town root
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}, nil
		}
		return nil, fmt.Errorf("reading town-tier formulas dir: %w", err)
	}

	result := make(map[string][]byte)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !hasFormulaSuffix(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(formulasDir, name)) //nolint:gosec // G304: name comes from the dir listing above
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		result[name] = data
	}
	return result, nil
}

// hasFormulaSuffix checks whether a filename already carries the
// .formula.toml suffix.
func hasFormulaSuffix(name string) bool {
	const suffix = ".formula.toml"
	return len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix
}

// formatByteSize renders a byte count the way the 2026-09-19 finding was
// reported ("30,870 B vs 36,443 B"): raw bytes under a kilobyte, rounded KB
// above.
func formatByteSize(n int) string {
	if n < 1024 {
		return strconv.Itoa(n) + " B"
	}
	return strconv.Itoa(n/1024) + " KB"
}