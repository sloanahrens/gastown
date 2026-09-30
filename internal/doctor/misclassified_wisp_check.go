package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// CheckMisclassifiedWisps detects ephemeral beads that are in the issues table
// instead of the wisps table. This is a data integrity check, not a heuristic —
// it only acts on beads whose ephemeral flag is already set (ZFC: agent decides,
// Go transports).
//
// Detection prefers Dolt (live DB via bd sql --csv) over JSONL, falling back to
// JSONL when the DB is unreachable.
type CheckMisclassifiedWisps struct {
	FixableCheck
	misclassified     []misclassifiedWisp
	misclassifiedRigs map[string]int // rig -> count
	// listDatabases lists the databases the town's Dolt server serves; nil
	// is doltserver.ListDatabases.
	listDatabases func(townRoot string) ([]string, error)
}

type misclassifiedWisp struct {
	rigName string
	workDir string
	id      string
	title   string
	reason  string
}

// NewCheckMisclassifiedWisps creates a new misclassified wisp check.
func NewCheckMisclassifiedWisps() *CheckMisclassifiedWisps {
	return &CheckMisclassifiedWisps{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "misclassified-wisps",
				CheckDescription: "Detect ephemeral beads misplaced in the issues table",
				CheckCategory:    CategoryCleanup,
			},
		},
		misclassifiedRigs: make(map[string]int),
	}
}

// Run checks for ephemeral beads in the issues table across all rigs.
// Only flags beads where ephemeral=1 — never guesses based on titles,
// labels, or ID patterns (ZFC compliance).
func (c *CheckMisclassifiedWisps) Run(ctx *CheckContext) *CheckResult {
	c.misclassified = nil
	c.misclassifiedRigs = make(map[string]int)

	// Try Dolt-first detection via ListDatabases (matches NullAssigneeCheck pattern).
	listDatabases := c.listDatabases
	if listDatabases == nil {
		listDatabases = doltserver.ListDatabases
	}
	databases, dbErr := listDatabases(ctx.TownRoot)
	useDolt := dbErr == nil && len(databases) > 0

	var details []string
	var totalProbeErrors int

	if useDolt {
		for _, db := range databases {
			rigDir := resolveMisclassifiedWispWorkDir(ctx.TownRoot, misclassifiedWisp{rigName: db})
			found, probeErrors := c.findMisplacedEphemeralsDolt(ctx, rigDir, db)
			totalProbeErrors += probeErrors
			if len(found) > 0 {
				c.misclassified = append(c.misclassified, found...)
				c.misclassifiedRigs[db] = len(found)
				details = append(details, fmt.Sprintf("%s: %d misplaced ephemeral(s)", db, len(found)))
			}
		}
	} else {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "Dolt unavailable — skipping misplaced ephemeral check",
		}
	}

	if totalProbeErrors > 0 {
		details = append(details, fmt.Sprintf("%d DB probe(s) failed — some databases were skipped", totalProbeErrors))
	}

	total := len(c.misclassified)
	if total > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d ephemeral bead(s) misplaced in issues table", total),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to migrate to wisps table",
		}
	}

	if totalProbeErrors > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "No misplaced ephemerals found (some DB probes failed)",
			Details: details,
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No misplaced ephemerals found",
	}
}

// findMisplacedEphemeralsDolt queries the live Dolt DB for beads in the issues
// table that have ephemeral=1. These should be in the wisps table instead.
// No heuristics — only the ephemeral flag matters.
func (c *CheckMisclassifiedWisps) findMisplacedEphemeralsDolt(ctx *CheckContext, rigDir, rigName string) ([]misclassifiedWisp, int) {
	issueQuery := `SELECT id, title FROM issues WHERE ephemeral = 1`
	issueRecords, err := runBdSQLCSV(ctx, rigDir, issueQuery)
	if err != nil {
		return nil, 1 // DB unavailable for this rig
	}
	if len(issueRecords) < 2 {
		return nil, 0
	}

	var found []misclassifiedWisp
	for _, rec := range issueRecords[1:] {
		if len(rec) < 2 {
			continue
		}
		found = append(found, misclassifiedWisp{
			rigName: rigName,
			workDir: rigDir,
			id:      strings.TrimSpace(rec[0]),
			title:   strings.TrimSpace(rec[1]),
			reason:  "ephemeral bead in issues table",
		})
	}

	return found, 0
}

// Fix moves each misplaced ephemeral bead from the issues table to the wisps
// table through bd (bd update --ephemeral), which carries its labels,
// comments, events and dependencies and journals the move (gt-fcxe9.12).
// A failed move does not stop the others.
func (c *CheckMisclassifiedWisps) Fix(ctx *CheckContext) error {
	var errs []string
	for _, w := range c.misclassified {
		workDir := resolveMisclassifiedWispWorkDir(ctx.TownRoot, w)
		if err := ctx.repair(workDir).DemoteToWisp(w.id); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", w.rigName, w.id, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("partial fix: %s", strings.Join(errs, "; "))
	}
	return nil
}

func resolveMisclassifiedWispWorkDir(townRoot string, w misclassifiedWisp) string {
	if w.workDir != "" {
		return w.workDir
	}

	if w.rigName == "town" || w.rigName == "hq" {
		return townRoot
	}

	if rigDir := beads.GetRigPathForPrefix(townRoot, w.rigName+"-"); rigDir != "" {
		return rigDir
	}

	return filepath.Join(townRoot, w.rigName)
}
