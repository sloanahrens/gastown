package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// What counts as dispatchable ready work: the beads-database guard the spec
// dispatcher's candidate scan reads, and the predicate the ready board's
// non-dispatchable-family tests hold it to. These lived in daemon_dispatch.go
// until the idle-seat dispatch check was deleted (gt-rwp7z.3).

// hasBeadsDatabase reports whether a resolved beads directory holds a database
// this check could read.
//
// It is a filesystem test on purpose. The one error the ready-work reads
// cannot absorb is under-reporting work, so an unreachable Dolt server must
// still surface as a read failure; asking whether a database is *configured*
// keeps the two apart (gt-ka00).
func hasBeadsDatabase(beadsDir string) bool {
	// Embedded mode: the data directory sits beside the config, and a .beads
	// directory carrying one need not carry a metadata.json to name it. bd
	// writes embeddeddolt/<database>/.dolt; dolt/ is the older layout.
	for _, embedded := range []string{"dolt", "embeddeddolt"} {
		if info, err := os.Stat(filepath.Join(beadsDir, embedded)); err == nil && info.IsDir() {
			return true
		}
	}

	// Server mode: metadata.json names the database and the server keeps it in
	// the town's .dolt-data/. A metadata.json tracked from another workspace
	// names a database this server may not have, which is the same empty rig
	// as one that was never initialized.
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json")) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return false
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return true // Unparseable — assume initialized, matching bdDatabaseExists
	}
	db := townconfig.DatabaseForBeadsDir(beadsDir)
	if meta.DoltMode != "server" || db == "" {
		return true // Not a server-mode database reference — assume initialized
	}
	townRoot := beads.FindTownRoot(filepath.Dir(beadsDir))
	if townRoot == "" {
		return true // No town to look in — assume initialized
	}
	_, err = os.Stat(filepath.Join(townRoot, ".dolt-data", db))
	return !os.IsNotExist(err)
}

// patrolSuppressedTitlePrefixes are notification envelopes this predicate does
// not count as dispatchable work, over and above beads.IsNonDispatchableBead.
//
// These are notices *about* work, not a kind of work, and they stay out of the
// shared predicate because the two callers can afford different mistakes here.
// A missed nudge costs silence until someone next reads the board; a hidden
// row costs the work itself, and the board still shows a "main_branch_test:
// <diagnosis>" bug — a real one is filed, gt-59yz — and a "STATE_COLLAPSE
// <rig>" notice asking for the work to be reopened and re-dispatched.
var patrolSuppressedTitlePrefixes = []string{
	"STATE_COLLAPSE",
	"[HIGH]",
	"[CRITICAL]",
	"[MEDIUM]",
	"main_branch_test:",
}

// isActionableReadyBead reports whether a ready bead is work a dispatcher could
// sling: at or above maxPriority's floor, and not one of the town's
// bookkeeping families. rv is the rig's revert in flight, which holds that
// rig's red-main beads.
//
// Epics are excluded as containers. `gt sling` accepts one, but a container's
// children are what a polecat takes, and they appear in ready on their own — a
// nudge naming the epic would point at the wrong row.
func isActionableReadyBead(issue *beads.Issue, maxPriority int, rv *specdispatch.Revert) bool {
	if issue == nil {
		return false
	}
	if issue.Priority < 0 || issue.Priority > maxPriority {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(issue.Type), "epic") {
		return false
	}
	if beads.IsNonDispatchableBead(issue) {
		return false
	}
	// Work the operator reserved is not the dispatcher's to sling: naming one
	// here is what sends a sling that refuses (gt-21pl0). This predicate and
	// the spec dispatcher's draw the same line the sling guard does.
	if dispatch.OperatorReservation(issue.Labels, issue.Assignee) != "" {
		return false
	}
	// A red-main bead is the fix forward for a breakage the rig's owner is
	// already undoing; counting it here nudges a sling of the very fix the
	// revert supersedes (gt-1fiv4). The rule is the spec dispatcher's own
	// (specdispatch.RedMainHold), so both hold the same beads (gt-zkdwt).
	if specdispatch.RedMainHold(specdispatch.Spec{Labels: issue.Labels}, rv) != "" {
		return false
	}

	title := strings.TrimSpace(issue.Title)
	for _, prefix := range patrolSuppressedTitlePrefixes {
		if strings.HasPrefix(title, prefix) {
			return false
		}
	}
	return true
}
