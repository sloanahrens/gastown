package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/role"
	"github.com/steveyegge/gastown/internal/style"
)

// A sling that follows a round of work is not a respawn, and the per-bead
// respawn limit must not spend the bead on it (gt-tatcn). The limit is enforced
// in polecat_spawn.go, which clears the rounds behind a submission here before
// it reads the count.
//
// What dates a round is the submission, not the label. The landing worker's
// rework label records that a refusal happened, never when, so on its own it
// cannot say which recorded spawns came before the refusal: read as evidence by
// itself it cleared the count on every sling of a rework bead whose polecat
// kept dying at start, which is the case the limit exists for. The submission
// the refusal followed is the same event with a timestamp on it.

// submissionCommentRE matches both shapes gt done leaves on submission: the
// branch form, and the bare "Submitted for landing (attempt N)" a bead with no
// readable READY TO LAND block gets (done_agent_state.go).
var submissionCommentRE = regexp.MustCompile(`^Submitted for landing\b`)

// latestSubmissionTime is when a bead was last submitted for landing: the live
// READY TO LAND block's stamp (gt done writes it, gt-t2jhf), or, for a bead
// submitted before the block carried one, its newest submission comment. Zero
// is "no submission this code can date", which a nil bead is too.
func latestSubmissionTime(issue *beads.Issue, comments []beads.Comment) time.Time {
	if issue == nil {
		return time.Time{}
	}
	if w, ok := land.ParseReadyNote(issue.Notes); ok && !w.Submitted.IsZero() {
		return w.Submitted
	}
	for i := len(comments) - 1; i >= 0; i-- {
		if !submissionCommentRE.MatchString(strings.TrimSpace(comments[i].Text)) {
			continue
		}
		if at := beads.ParseIssueTime(comments[i].CreatedAt); !at.IsZero() {
			return at
		}
	}
	return time.Time{}
}

// daemonMarker answers whether this process is the daemon or one it started.
// It is a field, not an environment read at the call, so a test states the
// answer instead of the machine's; nil is not the daemon.
type daemonMarker func() bool

func (m daemonMarker) active() bool { return m != nil && m() }

// slingDaemonProcess is the real daemonMarker: the daemon publishes
// BD_ACTOR=daemon to everything it spawns and strips the agent identity it
// inherited, so a sling from one of its children has no live role of its own
// (daemonGTEnv, GH#3006).
func slingDaemonProcess() bool { return os.Getenv("BD_ACTOR") == "daemon" }

// slingDeliberate reports whether a sling by actor is a hand dispatch: the
// operator at a terminal, where no agent role is detected, or a crew member —
// the overseer included — and never from the daemon's own process, whatever
// the actor says. Every other dispatcher is automated and spends the bead's
// respawn budget: a polecat or an automation role is what GT_ROLE holds, the
// daemon names itself in --actor, and an actor this rule does not recognise
// counts as automated too, so a dispatcher added later is held to the limit
// until it is classified here on purpose (gt-jogkx).
func slingDeliberate(actor string, underDaemon bool) bool {
	if underDaemon {
		return false
	}
	switch r, _, _ := role.Parse(actor); r {
	case role.Crew, role.Unknown:
		return true
	default:
		return false
	}
}

// admitSlingRespawn decides whether a sling may pass the per-bead respawn
// limit. It returns nil when the sling may proceed and the refusal to report
// when the bead has spent its budget.
//
// A deliberate sling is admitted whatever the count says: the operator or crew
// member reworking the bead has made the decision the limit guards, and the
// count is cleared once a seat exists (recordSlingRespawn), so an unreadable
// state file does not stand between them and the work. An automated sling is
// held to the limit — the rounds behind a submission for landing are cleared
// first (gt-tatcn), and then the refusal stands — and --force overrides it
// without spending or clearing anything (gt-jogkx). stores is where the bead's
// submission is read from; the spawn path passes the real one.
func admitSlingRespawn(stores slingStores, townRoot, rigName string, opts SlingSpawnOptions) error {
	if opts.HookBead == "" || opts.Force || opts.Deliberate {
		return nil
	}
	// A sling that follows a round which submitted is a round of work, not a
	// failed start, so the rounds behind that submission are cleared before
	// the limit reads them. The discount runs before the read because the limit
	// is about deaths in the round this sling starts, and the counts it drops
	// are the rounds behind the submission, which are not deaths by that rule.
	// A sling that then fails to launch records no spawn of its own, so it
	// costs the bead nothing it would be blocked on.
	clearRespawnForSubmittedRound(stores, townRoot, opts.HookBead)
	if !polecat.ShouldBlockRespawn(townRoot, opts.HookBead) {
		return nil
	}
	maxRespawns := config.LoadOperationalConfig(townRoot).GetRecoveryConfig().MaxBeadRespawnsV()
	return fmt.Errorf("respawn limit reached for %s (%d attempts). "+
		"This bead keeps failing — investigate before re-dispatching.\n"+
		"Override: gt sling %s %s --force\n"+
		"Reset:    gt sling respawn-reset %s",
		opts.HookBead, maxRespawns,
		opts.HookBead, rigName, opts.HookBead)
}

// recordSlingRespawn records one spawn of the sling's bead against its respawn
// budget and returns the mark of the increment.
//
// A deliberate sling is not a respawn: it clears the bead's count instead of
// spending it, and does so here rather than at the gate, so a sling that never
// reaches a polecat leaves the count it found. An automated sling spends the
// budget. --force does neither: the override gets past the limit without
// touching the count (gt-jogkx).
//
// A count that cannot be written is reported and the spawn keeps its seat: the
// polecat is already attached, and the count it leaves behind is the safe
// direction — a deliberate sling leaves the bead as it found it, and an
// automated one stays at its limit rather than silently re-arming (gt-u3hc1).
func recordSlingRespawn(townRoot string, opts SlingSpawnOptions) polecat.RespawnMark {
	if opts.HookBead == "" || opts.Force {
		return polecat.RespawnMark{}
	}
	if opts.Deliberate {
		if err := polecat.ResetBeadRespawnCount(townRoot, opts.HookBead); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: clearing the respawn count of %s: %v\n", opts.HookBead, err)
		}
		return polecat.RespawnMark{}
	}
	mark, err := polecat.RecordBeadRespawnMark(townRoot, opts.HookBead)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: recording respawn of %s: %v\n", opts.HookBead, err)
		return polecat.RespawnMark{}
	}
	return mark
}

// clearRespawnForSubmittedRound clears beadID's respawn count when this sling
// follows a round that reached a submission for landing, before the limit reads
// it. A bead whose submission cannot be read or dated clears nothing: the count
// is the breaker for a polecat that dies at start over and over, and keeping it
// is the safe direction.
func clearRespawnForSubmittedRound(stores slingStores, townRoot, beadID string) {
	if beadID == "" {
		return
	}
	issue, err := stores.show(townRoot, beadID)
	if err != nil || issue == nil {
		return
	}
	comments, err := stores.comments(townRoot, beadID)
	if err != nil {
		// Only the submission comments are lost. A bead whose READY TO LAND
		// block carries a stamp still dates its round, so this is reported and
		// not returned on.
		style.PrintWarning("could not read the comments on %s: %v", beadID, err)
		comments = nil
	}
	submitted := latestSubmissionTime(issue, comments)
	if submitted.IsZero() {
		return
	}
	cleared, err := polecat.ResetBeadRespawnForRound(townRoot, beadID, submitted)
	if err != nil {
		style.PrintWarning("could not clear the respawn count for %s: %v", beadID, err)
		return
	}
	if cleared {
		fmt.Printf("  %s Cleared the count for %s: it is a round that submitted, not a respawn\n", style.Bold.Render("✓"), beadID)
	}
}
