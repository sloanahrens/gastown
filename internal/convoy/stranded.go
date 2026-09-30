package convoy

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// StrandedConvoy is a convoy with ready work but no workers, or an empty
// convoy (0 tracked issues) that needs cleanup.
type StrandedConvoy struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	TrackedCount int      `json:"tracked_count"`
	ReadyCount   int      `json:"ready_count"`
	ReadyIssues  []string `json:"ready_issues"`
	// Held lists the otherwise-ready beads the scan kept back because a
	// blocker could not be resolved or read, not because one is open. The
	// daemon escalates a hold that lasts (gt-gg7w9).
	Held       []StrandedHold `json:"held,omitempty"`
	CreatedAt  string         `json:"created_at,omitempty"`
	BaseBranch string         `json:"base_branch,omitempty"`
	// Agent is the runtime agent requested when the convoy's beads were slung
	// (--agent). Feeders must re-dispatch with it instead of the rig default,
	// so a failed sling cannot silently re-route the bead (gt-yg24).
	Agent string `json:"agent,omitempty"`
	// Formula is the formula requested when the convoy's beads were slung
	// (--formula). Feeders must re-dispatch with it instead of the rig
	// default formula, so a sling whose formula bond fails and rolls back
	// cannot be silently re-fed under the wrong formula (gt-4lor).
	Formula string `json:"formula,omitempty"`
	// Owned reports whether the convoy carries the gt:owned label. Owned
	// convoys have a designated owner responsible for their own dispatch
	// cadence; the system-managed stranded scan (daemon's feedFirstReady)
	// must not auto-feed them (gt-qw4u).
	Owned bool `json:"owned,omitempty"`
}

// StrandedHold is one bead held on a fail-safe verdict from BlockOf. The
// daemon reads it from `gt convoy stranded --json`.
type StrandedHold struct {
	Issue string `json:"issue"`
	// Blocker is "" when the failed read was the bead's own.
	Blocker string `json:"blocker,omitempty"`
	// Cause is "unresolved" (no store has the blocker) or "unreadable" (a
	// store would not answer).
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

// FindStranded finds convoys with ready work but no workers, or empty convoys
// (0 tracked issues) that need cleanup.
func (t Town) FindStranded(ctx context.Context) ([]StrandedConvoy, error) {
	return t.findStrandedWith(ctx, func(townRoot string) (blockCheck, func(), error) {
		return openStrandedBlockCheck(ctx, townRoot)
	})
}

// blockCheck returns why a bead's dependencies keep it from dispatch; the zero
// Block when none does.
type blockCheck func(issueID string) Block

// openStrandedBlockCheck is the production blocker check for the stranded
// scan: BlockOf over the town store and a resolver that opens each rig's store
// on first use. bd show cannot be used for this: its dependencies join each
// edge to an issue row in the bead's own database and drop every cross-rig
// blocker, so the daemon's stranded feed slung beads another rig's open bead
// blocked (gt-j02xy). A town store that will not open is an error: the scan
// fails rather than hold every bead in silence.
func openStrandedBlockCheck(ctx context.Context, townRoot string) (blockCheck, func(), error) {
	return openStrandedBlockCheckWith(ctx, townRoot, func(beadsDir string) (beadsdk.Storage, error) {
		return beads.OpenStoreFromConfig(ctx, beadsDir)
	})
}

// openStrandedBlockCheckWith is openStrandedBlockCheck with the store opener
// supplied: openStore opens the beads store in a .beads directory.
func openStrandedBlockCheckWith(ctx context.Context, townRoot string, openStore func(beadsDir string) (beadsdk.Storage, error)) (blockCheck, func(), error) {
	townStore, err := openStore(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return nil, nil, fmt.Errorf("town beads store unavailable: %w", err)
	}
	resolver := NewOpeningStoreResolver(townRoot, func(name string) (beadsdk.Storage, error) {
		beadsDir := doltserver.FindRigBeadsDir(townRoot, name)
		if beadsDir == "" {
			return nil, fmt.Errorf("no beads directory for rig %s", name)
		}
		return openStore(beadsDir)
	})
	check := func(issueID string) Block {
		return BlockOf(ctx, townStore, issueID, resolver)
	}
	return check, func() {
		_ = resolver.Close()
		_ = townStore.Close()
	}, nil
}

// findStrandedWith is FindStranded with the blocker check's opener supplied.
// openCheck runs at most once, and only when some convoy has a bead that is
// otherwise ready, so a scan with nothing to feed opens no store.
func (t Town) findStrandedWith(ctx context.Context, openCheck func(townRoot string) (blockCheck, func(), error)) ([]StrandedConvoy, error) {
	stranded := []StrandedConvoy{} // Initialize as empty slice for proper JSON encoding
	// The blocker check opens on the first otherwise-ready bead.
	var check blockCheck
	release := func() {}
	defer func() { release() }()

	convoys, err := t.ListConvoys("open", false)
	if err != nil {
		return nil, fmt.Errorf("listing convoys: %w", err)
	}

	// Check each convoy for stranded state
	for _, convoy := range convoys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Extract base_branch, agent, and formula from convoy description fields
		var baseBranch, convoyAgent, convoyFormula string
		if cf := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description}); cf != nil {
			baseBranch = cf.BaseBranch
			convoyAgent = cf.Agent
			convoyFormula = cf.Formula
		}
		owned := HasLabel(convoy.Labels, OwnedLabel)

		tracked, err := t.TrackedIssues(convoy.ID)
		if err != nil {
			// Warn, never print: the daemon and `--json` read stdout as JSON
			// (fixes #2142).
			fmt.Fprintf(t.warnWriter(), "⚠ Warning: skipping convoy %s: %v\n", convoy.ID, err)
			continue
		}
		// Empty convoys (0 tracked issues) are stranded — they need
		// attention (auto-close via convoy check or manual cleanup).
		if len(tracked) == 0 {
			stranded = append(stranded, StrandedConvoy{
				ID:           convoy.ID,
				Title:        convoy.Title,
				TrackedCount: 0,
				ReadyCount:   0,
				ReadyIssues:  []string{},
				CreatedAt:    convoy.CreatedAt,
				BaseBranch:   baseBranch,
				Agent:        convoyAgent,
				Formula:      convoyFormula,
				Owned:        owned,
			})
			continue
		}

		// Find ready issues (open, not blocked, no live assignee, slingable).
		// Town-level beads (hq- prefix with path=".") are excluded because
		// they can't be dispatched via gt sling -- they're handled by the deacon.
		// Non-slingable types (epics, convoys, etc.) are also excluded.

		// Batch-check scheduling status for all tracked issues (single DB query).
		var trackedIDs []string
		for _, tr := range tracked {
			trackedIDs = append(trackedIDs, tr.ID)
		}
		scheduledSet := beads.AreScheduledWith(t.Root, trackedIDs, t.Run)

		var readyIssues []string
		var held []StrandedHold
		for _, tr := range tracked {
			if !isReadyIssue(tr, scheduledSet) {
				continue
			}
			if !isSlingableBead(t.Root, tr.ID) {
				continue
			}
			if !IsSlingableType(tr.IssueType) {
				continue
			}
			if check == nil {
				opened, releaseOpened, openErr := openCheck(t.rootDir())
				if openErr != nil {
					// Holding every candidate in silence would read, to the
					// daemon, as "N tracked, 0 ready" on every scan: it
					// drops a successful run's stderr. Failing the scan
					// makes it log "stranded scan failed" with the cause.
					return nil, fmt.Errorf("blocker check: %w", openErr)
				}
				check, release = opened, releaseOpened
			}
			if block := check(tr.ID); block.Reason != "" {
				// Warn, never print: stdout is the daemon's JSON (#2142).
				fmt.Fprintf(t.warnWriter(), "convoy %s: %s not ready: blocked (%s)\n", convoy.ID, tr.ID, block.Reason)
				// A fail-safe hold is named in the JSON too: the daemon
				// drops a successful run's stderr, and this is what it
				// escalates from (gt-gg7w9).
				if block.Held() {
					held = append(held, StrandedHold{
						Issue:   tr.ID,
						Blocker: block.BlockerID,
						Cause:   string(block.Cause),
						Reason:  block.Reason,
					})
				}
				continue
			}
			readyIssues = append(readyIssues, tr.ID)
		}

		// A convoy with tracked issues but none ready stays in the list so
		// callers can tell it from a truly empty convoy.
		if readyIssues == nil {
			readyIssues = []string{}
		}
		stranded = append(stranded, StrandedConvoy{
			ID:           convoy.ID,
			Title:        convoy.Title,
			TrackedCount: len(tracked),
			ReadyCount:   len(readyIssues),
			ReadyIssues:  readyIssues,
			Held:         held,
			CreatedAt:    convoy.CreatedAt,
			BaseBranch:   baseBranch,
			Agent:        convoyAgent,
			Formula:      convoyFormula,
			Owned:        owned,
		})
	}

	return stranded, nil
}

// isReadyIssue checks if an issue is ready for dispatch (stranded).
// Readiness is an allowlist of statuses, not a list of the ones that are not
// ready (gt-t08jn): anything else — blocked, deferred, pinned, a custom status
// — is work the tracker says is not ready, assigned or not. An issue is ready
// if it is not scheduled, and:
//   - status = "open" AND (no assignee OR assignee session is dead)
//   - OR status = "in_progress"/"hooked" AND (no assignee OR assignee session is
//     dead) — an orphaned molecule, whose recovery is a re-dispatch
//
// scheduledSet is a pre-computed set of bead IDs with open sling contexts (from beads.AreScheduled).
func isReadyIssue(t TrackedIssue, scheduledSet map[string]bool) bool {
	status := beads.IssueStatus(strings.TrimSpace(t.Status))
	if status != beads.StatusOpen && !status.IsAssigned() {
		return false
	}

	// Dependency blockers are not decided here: bd show drops a blocker in
	// another rig, so findStrandedWith asks BlockOf, the rule the continuation
	// feed uses, once the bead is otherwise ready (gt-j02xy).

	// Scheduled beads are not stranded — they're waiting for dispatch capacity.
	if scheduledSet[t.ID] {
		return false
	}

	// Work submitted for landing is not stranded: its session ended on
	// purpose and the landing worker owns it (gt-v4ssj.2).
	if slices.Contains(t.Labels, land.LabelReadyToLand) {
		return false
	}

	// No assignee: an open issue is trivially ready, and an in_progress/hooked
	// one is a molecule that detached improperly and needs re-dispatch.
	if t.Assignee == "" {
		return true
	}

	sessionName, _ := session.AssigneeSessionName(t.Assignee)
	if sessionName == "" {
		return true // Can't determine session = treat as ready
	}

	// Check if tmux session exists
	checkCmd := tmux.BuildCommand("has-session", "-t", sessionName)
	if err := checkCmd.Run(); err != nil {
		// Session doesn't exist = orphaned molecule or dead worker
		// This is the key fix: issues with in_progress/hooked status but
		// dead workers are now correctly detected as stranded
		return true
	}

	return false // Session exists = worker is active
}

// isSlingableBead reports whether a bead can be dispatched via gt sling.
// Town-level beads (hq- prefix with path=".") and beads with unknown
// prefixes are not slingable — they're handled by the deacon/mayor.
func isSlingableBead(townRoot, beadID string) bool {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return true // No prefix info, assume slingable
	}
	return beads.GetRigNameForPrefix(townRoot, prefix) != ""
}
