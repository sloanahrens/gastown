package polecat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// sandboxSeatTimeout bounds the one gateway question a seat's git state asks.
const sandboxSeatTimeout = 15 * time.Second

// SandboxSeatProbe answers the git state of a sandbox seat: agent is the agent
// the polecat's agent bead recorded and sessionName its session, which is also
// its sandbox's name. ok is false for a host seat, whose worktree the caller
// measures as before. A sandbox seat's host worktree is a stale leftover (its
// commits were pushed from inside the sandbox), so it is never measured: the
// gateway's answer decides (SandboxSeatGitState, gt-qq6rg).
type SandboxSeatProbe func(agent, sessionName string) (state LiveGitState, ok bool)

// NewSandboxSeatProbe reads the seat map of the town at townRoot once, on the
// first question, and asks the gateway through exists (nil means the sandbox
// CLI). A settings file or seat map that cannot be read leaves every seat
// unclassifiable, so each answers unknown rather than falling back to a host
// seat's measured worktree, which for a sandbox seat is the stale leftover this
// probe exists to avoid. The daemon's GitState fails closed the same way.
//
// A seat is found by agent name alone because polecat_pool.seats is keyed by
// agent (config.Seats yields one seat per agent), so the recorded agent names
// exactly one seat. The sandbox is named for the seat's session, as the
// daemon's GitState and the reaper name it.
func NewSandboxSeatProbe(townRoot string, exists func(gateway, name string) (bool, error)) SandboxSeatProbe {
	if exists == nil {
		exists = func(gateway, name string) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), sandboxSeatTimeout)
			defer cancel()
			return (&sandbox.CLI{Gateway: gateway}).Exists(ctx, name)
		}
	}
	var (
		once    sync.Once
		ts      *config.TownSettings
		seats   []config.Seat
		loadErr error
	)
	load := func() {
		ts, loadErr = config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
		if loadErr != nil {
			loadErr = fmt.Errorf("reading town settings: %w", loadErr)
			return
		}
		if seats, loadErr = config.Seats(ts, nil); loadErr != nil {
			loadErr = fmt.Errorf("reading polecat_pool.seats: %w", loadErr)
		}
	}
	return func(agent, sessionName string) (LiveGitState, bool) {
		if agent == "" {
			return LiveGitState{}, false
		}
		once.Do(load)
		if loadErr != nil {
			return LiveGitState{
				Source:       GitStateSourceUnknown,
				FailedReason: fmt.Sprintf("could not tell whether agent %s is a sandbox seat: %v", agent, loadErr),
			}, true
		}
		for _, seat := range seats {
			if seat.Agent != agent {
				continue
			}
			if seat.Backend != config.BackendOpenShell {
				return LiveGitState{}, false
			}
			gateway := ts.Sandboxes[seat.Sandbox].WithDefaults().Gateway
			found, existsErr := exists(gateway, sessionName)
			return SandboxSeatGitState("", found, existsErr, sessionName), true
		}
		return LiveGitState{}, false
	}
}

// sandboxSeatState is the sandbox probe's answer for the polecat's agent bead:
// a nil probe, or a polecat with no recorded agent bead, is a host seat.
func (m *Manager) sandboxSeatState(fields *beads.AgentFields, name string) (LiveGitState, bool) {
	if fields == nil {
		return LiveGitState{}, false
	}
	probe := m.sandboxSeat
	if probe == nil {
		if m.townRoot == "" {
			return LiveGitState{}, false
		}
		m.sandboxSeatOnce.Do(func() { m.sandboxSeatDefault = NewSandboxSeatProbe(m.townRoot, nil) })
		probe = m.sandboxSeatDefault
	}
	return probe(fields.Agent, m.sessionName(name))
}

// applySandboxSeatFacts puts a sandbox seat's gateway-derived git state onto
// facts. The branch is the one the agent bead recorded (what the sandbox
// pushed), and a state the gateway could not give stays unknown.
func applySandboxSeatFacts(facts *WorkstateFacts, live LiveGitState, fields *beads.AgentFields) {
	live.ApplyFacts(facts)
	if facts.Branch == "" && fields != nil {
		facts.Branch = fields.Branch
	}
}

// sandboxSeatNoBranchReason is why a sandbox seat that recorded no branch has
// no branch to look for on origin. It opens every fail-closed answer this
// package gives such a seat, with the case that kept it closed appended
// (gt-cu66c, gt-xhnn3).
const sandboxSeatNoBranchReason = "the agent bead records no branch to look for on origin"

// SandboxSeatOriginBranch is the branch the lost-work checks ask origin about
// for a gone sandbox seat — the one its agent bead recorded — and, for a bead
// that recorded none, the reason the answer is unknown, which is never the host
// polecat's branch because that names a worktree the sandbox never wrote to
// (gt-cu66c).
func SandboxSeatOriginBranch(fields *beads.AgentFields) (branch, unknownReason string) {
	if fields != nil {
		if branch = strings.TrimSpace(fields.Branch); branch != "" {
			return branch, ""
		}
	}
	return "", sandboxSeatNoBranchReason
}

// SandboxSeatLandedWithoutBranch answers the lost-work question for a sandbox
// seat whose agent bead recorded no branch, and so names nothing to ask origin
// about. Such a seat is finished, and nothing it did is at risk, only when its
// recorded issue is closed with a landing record and no branch named for the
// seat and encoding that issue survives on origin (gt-xhnn3).
//
// It returns "" for that safe case and the fail-closed reason otherwise: an
// issue that is not closed, an issue closed with no landing record (a
// no-changes close is not a landing), a seat name that cannot be established,
// an origin that cannot be asked, or a branch for the seat and issue still on
// origin. A nil issue is a bead that no longer exists, which leaves nothing at
// risk, the answer both callers already give a Show that reported it missing
// (gt-qq6rg.4). Every caller reports a non-empty reason as an unknown git
// state, which is the verdict this case got before the landed case was
// recognized.
//
// seatName is the name the seat's branches are generated under
// (polecat/<seat>/<issue>). originBranches reports every generated polecat
// branch on origin that encodes issueID, most recently generated first; the
// rule, not the seam, keeps only the ones named for seatName, so the manager
// and check-recovery reach the same verdict through their own git handles
// (gt-cu66c).
func SandboxSeatLandedWithoutBranch(seatName string, issue *beads.Issue, issueID string, originBranches func(issueID string) ([]string, error)) string {
	if issue == nil {
		return "" // the bead is gone: no unfinished work to lose
	}
	if beads.IssueStatus(issue.Status) != beads.StatusClosed {
		status := string(beads.IssueStatus(issue.Status))
		if status == "" {
			status = "unrecorded"
		}
		return sandboxSeatNoBranchReason + ": " + issueID + " is " + status
	}
	if _, landed := land.ParseLandingRecord(issue.Notes); !landed {
		return sandboxSeatNoBranchReason + ": " + issueID + " is closed with no landing record"
	}
	seatName = strings.TrimSpace(seatName)
	if seatName == "" {
		return sandboxSeatNoBranchReason + ": no seat name to match a branch for " + issueID
	}
	branches, err := originBranches(issueID)
	if err != nil {
		return fmt.Sprintf("could not ask origin whether a branch for %s survived: %v", issueID, err)
	}
	for _, branch := range branches {
		if meta, ok := ParseGeneratedBranchName(branch); ok && meta.Polecat == seatName {
			return sandboxSeatNoBranchReason + ": " + branch + " still exists on origin"
		}
	}
	return ""
}

// applySandboxLostWorkFacts sets the facts the lost-work verdict reads once the
// gateway says the sandbox is gone: the bead the seat was working (hook, else
// source issue) and whether a resume_branch on it or the seat's branch on origin
// survives. Nothing is read from the stale host worktree. A bead, remote or
// recorded branch that cannot be read or found leaves the git state unknown, never
// lost or clean; a bead that no longer exists leaves nothing at risk (gt-qq6rg.4).
// seatName is the polecat's name, which the branch of a seat that recorded none
// is matched under (gt-xhnn3).
func (m *Manager) applySandboxLostWorkFacts(facts *WorkstateFacts, fields *beads.AgentFields, sourceHint, seatName string) {
	if fields == nil {
		return
	}
	workBead := strings.TrimSpace(fields.HookBead)
	if workBead == "" {
		workBead = strings.TrimSpace(sourceHint)
	}
	if workBead == "" {
		return
	}
	unknown := func(reason string) {
		facts.GitCheckFailed = true
		facts.GitCheckFailedReason = "git_state=unknown: " + reason
		facts.GitStateSource = GitStateSourceUnknown
	}
	preserved := false
	issue, showErr := m.beads.Show(workBead)
	switch {
	case errors.Is(showErr, beads.ErrNotFound):
		return // the bead is gone: no unfinished work to lose
	case showErr != nil:
		unknown(fmt.Sprintf("could not read %s for a resume_branch: %v", workBead, showErr))
		return
	}
	if issue != nil && patrolscan.ResumeBranchFromNotes(issue.Notes) != "" {
		preserved = true
	} else {
		branch, unknownReason := SandboxSeatOriginBranch(fields)
		switch {
		case unknownReason != "":
			// A seat that recorded no branch names nothing to ask origin
			// about. It is judged by its issue instead: one that landed with
			// no branch of its own left on origin has nothing at risk, and
			// every other answer is the refusal this case always got
			// (gt-xhnn3).
			if reason := SandboxSeatLandedWithoutBranch(seatName, issue, workBead, func(issueID string) ([]string, error) {
				return findSurvivingBranchesForIssue(m.gits, m.rig.Path, issueID)
			}); reason != "" {
				unknown(reason)
				return
			}
			preserved = true
		default:
			onOrigin, err := branchOnOrigin(m.gits, m.rig.Path, branch)
			if err != nil {
				unknown(fmt.Sprintf("could not ask origin whether %s was pushed: %v", branch, err))
				return
			}
			preserved = onOrigin
		}
	}
	facts.SandboxGone = true
	facts.SandboxWorkBead = workBead
	// Preserved means nothing here is at risk: the branch is on origin, a bead
	// names a resume_branch, or — with no branch recorded at all — the issue
	// landed and no branch for it survives (gt-xhnn3).
	facts.SandboxBranchPreserved = preserved
}
