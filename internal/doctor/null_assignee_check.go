package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// NullAssigneeCheck detects in_progress beads with a NULL or empty assignee.
//
// These records arise from partial writes (crash mid-transaction or race
// condition during bd update). They are invisible to bd show / bd list because
// bd's deserialization fails silently on a NULL assignee, so the steps remain
// stuck in_progress indefinitely and block molecule progress.
//
// Detection uses bd sql --csv (a read-only raw SQL passthrough, not affected
// by bd's ORM). Fix resets each affected bead to open with no assignee through
// bd update in machine mode, so the step can be re-dispatched and bd records
// the repair (gt-fcxe9.12).
type NullAssigneeCheck struct {
	FixableCheck
	affected []nullAssigneeRow
}

type nullAssigneeRow struct {
	ID        string
	Title     string
	UpdatedAt string
	RigDB     string // registered rig name
}

// NewNullAssigneeCheck creates a new null-assignee steps check.
func NewNullAssigneeCheck() *NullAssigneeCheck {
	return &NullAssigneeCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "null-assignee-steps",
				CheckDescription: "Check for in_progress beads with NULL assignee (invisible to bd, blocking indefinitely)",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run queries each registered rig's database for in_progress beads with
// NULL/empty assignee. It goes by mayor/rigs.json, not the Dolt server's
// database list: database names are bead prefixes (gt, be), not rig
// directories, and the server also hosts trackers that are no rig, such as
// the operator's own, whose in_progress epics carry no assignee by design.
//
// A rig the town route table puts on the tracker is not read: the tracker's
// assignee column is NOT NULL with an empty-string default
// (tracker/store/schema/0001_core.sql), so no row can carry the NULL this
// check looks for, and the backend refuses the sql passthrough. The result
// names each skipped rig instead of reporting OK having read nothing. An
// unreadable route table is a warning, and every rig is read as before.
func (c *NullAssigneeCheck) Run(ctx *CheckContext) *CheckResult {
	c.affected = nil

	registered := loadRegisteredRigNames(ctx.TownRoot)
	if len(registered) == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "No rig databases found (skipping)",
			Category: c.Category(),
		}
	}
	rigs := make([]string, 0, len(registered))
	for rig := range registered {
		rigs = append(rigs, rig)
	}
	sort.Strings(rigs)

	trackerRigs, routesErr := trackerRigNames(ctx.TownRoot)
	routesDetail := ""
	if routesErr != nil {
		// The route table names the rigs the tracker holds. Without it every
		// rig is read as before (a tracker refusal drops out with the other
		// read errors), and the result says the table could not be read so
		// the run cannot pass as clean.
		trackerRigs = nil
		routesDetail = fmt.Sprintf(
			"cannot read the town routes, so tracker-routed rigs are not skipped: %v", routesErr)
	}

	var skipped []string
	for _, rig := range rigs {
		if trackerRigs[rig] {
			skipped = append(skipped, rig)
			continue
		}
		rigDir := filepath.Join(ctx.TownRoot, rig)
		rows, err := queryNullAssigneeBeads(ctx, rigDir)
		if err != nil {
			// Non-fatal: Dolt might not be running.
			continue
		}
		for _, row := range rows {
			row.RigDB = rig
			c.affected = append(c.affected, row)
		}
	}

	skipDetails := skippedTrackerDetails(skipped)

	if len(c.affected) == 0 {
		res := &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "No in_progress beads with NULL assignee found",
			Category: c.Category(),
		}
		if len(skipped) > 0 {
			res.Message = fmt.Sprintf(
				"No in_progress beads with NULL assignee found (skipped %d tracker rig(s))", len(skipped))
			res.Details = skipDetails
		}
		if routesErr != nil {
			res.Status = StatusWarning
			res.Message = "No in_progress beads with NULL assignee found, but the town routes could not be read"
			res.Details = append([]string{routesDetail}, res.Details...)
		}
		return res
	}

	details := make([]string, 0, len(c.affected)+len(skipDetails)+1)
	for _, row := range c.affected {
		details = append(details, fmt.Sprintf("[%s] %s — %s (updated: %s)",
			row.RigDB, row.ID, shortenTitle(row.Title, 60), row.UpdatedAt))
	}
	details = append(details, skipDetails...)
	if routesErr != nil {
		details = append(details, routesDetail)
	}

	message := fmt.Sprintf(
		"%d in_progress bead(s) with NULL assignee — invisible to bd, blocking molecule progress",
		len(c.affected),
	)
	if len(skipped) > 0 {
		message += fmt.Sprintf(" (skipped %d tracker rig(s))", len(skipped))
	}
	if routesErr != nil {
		message += " (the town routes could not be read)"
	}

	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusWarning,
		Message:  message,
		Details:  details,
		FixHint:  "Run 'gt doctor fix null-assignee-steps' to reset to open for re-dispatch",
		Category: c.Category(),
	}
}

// DestructiveFix marks this repair as destructive (gt-638go.3): it resets in-progress tracker rows.
func (c *NullAssigneeCheck) DestructiveFix() bool { return true }

// Fix resets each affected bead to open with no assignee through bd, in the
// database it was found in. Only the ids Run found are touched, and a failed
// repair does not stop the others.
func (c *NullAssigneeCheck) Fix(ctx *CheckContext) error {
	var errs []string
	for _, row := range c.affected {
		rigDir := filepath.Join(ctx.TownRoot, row.RigDB)
		err := ctx.repair(rigDir).ReopenUnassigned(row.ID)
		if errors.Is(err, beads.ErrGuardNotHeld) {
			// Claimed or moved on since Run found it: no longer ours to repair.
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", row.RigDB, row.ID, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("partial fix: %s", strings.Join(errs, "; "))
	}
	return nil
}

// queryNullAssigneeBeads returns in_progress beads with NULL/empty assignee for a rig.
// Uses bd sql --csv (raw SQL passthrough, not affected by bd ORM deserialization).
func queryNullAssigneeBeads(ctx *CheckContext, rigDir string) ([]nullAssigneeRow, error) {
	records, err := runBdSQLCSV(ctx, rigDir, beadsql.UnassignedInProgress())
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, nil // No results or empty table
	}

	rows := make([]nullAssigneeRow, 0, len(records)-1)
	for _, rec := range records[1:] { // Skip CSV header
		if len(rec) < 3 {
			continue
		}
		rows = append(rows, nullAssigneeRow{
			ID:        strings.TrimSpace(rec[0]),
			Title:     strings.TrimSpace(rec[1]),
			UpdatedAt: strings.TrimSpace(rec[2]),
		})
	}
	return rows, nil
}

// trackerRigNames returns the registered rigs the town route table puts on the
// tracker. A route path names its rig by the first path component, as
// rig_beads_check reads it; the town's own route (".") is no rig.
func trackerRigNames(townRoot string) (map[string]bool, error) {
	paths, err := trackerRoutedRigs(townRoot)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(paths))
	for _, p := range paths {
		name := strings.SplitN(filepath.ToSlash(filepath.Clean(p)), "/", 2)[0]
		if name == "" || name == "." {
			continue
		}
		names[name] = true
	}
	return names, nil
}

// skippedTrackerDetails is the one detail line each skipped tracker rig
// contributes, so the run says what it did not read.
func skippedTrackerDetails(rigs []string) []string {
	details := make([]string, 0, len(rigs))
	for _, rig := range rigs {
		details = append(details, fmt.Sprintf("skipped tracker rig %s: the tracker stores no NULL assignee", rig))
	}
	return details
}

// shortenTitle truncates a title with ellipsis if it exceeds n runes.
func shortenTitle(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}
