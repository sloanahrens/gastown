// Package cmd provides polecat spawning utilities for gt sling.
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

const minPolecatDirsPerRig = 30

// SpawnedPolecatInfo contains info about a spawned polecat session.
type SpawnedPolecatInfo struct {
	RigName     string // Rig name (e.g., "gastown")
	PolecatName string // Polecat name (e.g., "Toast")
	ClonePath   string // Path to polecat's git worktree
	SessionName string // Tmux session name (e.g., "gt-gastown-p-Toast")
	Pane        string // Tmux pane ID (empty until StartSession is called)
	BaseBranch  string // Effective MERGE-TARGET base branch (e.g., "main", "integration/epic-id").
	// gt-a8i3: this must NEVER be the resume branch — it feeds the base_branch
	// formula var, which `gt done`/`gt mq submit` read as the MR --target. A
	// resume dispatch checks out ResumeBranch as the polecat's working branch
	// (see Branch below) but still merges back to the rig's normal target.
	Branch string // Git branch name actually checked out (for cleanup on rollback; equals ResumeBranch on a resume dispatch)

	// Provenance for rollback (gt-7evi4). A rollback undoes only what this
	// sling created, so both fields default to the safe answer: false means
	// "not ours", and the sandbox or branch is kept.
	//
	// FreshSpawn is true when this sling allocated the polecat's sandbox. A
	// reused persistent sandbox (gt-4ac) is never removed by a rollback.
	FreshSpawn bool
	// BranchCreated is true when this sling created Branch. A resumed branch
	// (--branch / --pr) existed before the sling and is never deleted by it.
	BranchCreated bool

	// HookBead is the bead this spawn was made for; empty when unknown. A
	// failed session start is recorded against it so the automatic
	// dispatchers back off (gt-wacl).
	HookBead string

	// Internal fields for deferred session start
	account string
	agent   string

	// townRoot is the town this spawn belongs to, recorded by its spawner. It
	// is what startSession and noteStartOutcome read the workspace from: the
	// daemon's scheduled dispatch starts sessions in process, and a session
	// started against whichever town the daemon's cwd names is the wrong town.
	// Empty falls back to the cwd, which is what a test-built spawn has.
	townRoot string

	// originalHold is the work bead's status and assignee before this sling
	// touched it; nil when unknown. A rollback that finds the bead's work
	// surviving hands it back to this holder instead of releasing it.
	originalHold *beadHold

	// seatClaim is the pool seat this spawn reserved, and the store it lives
	// in. It travels with the spawn because that is what both ends of the
	// claim's life have in hand: startSession drops it once the tmux session
	// exists, and the rollback drops it when no session ever will. Nil when
	// the spawn did not go through the pool's seat decision (gt-t8q5).
	seatClaim *poolSeatClaimStore
}

// releaseSeatClaim drops the pool seat this spawn reserved. Idempotent, and a
// no-op for a spawn that reserved none.
func (s *SpawnedPolecatInfo) releaseSeatClaim() {
	if s == nil {
		return
	}
	s.seatClaim.release()
}

// beadHold is a work bead's status and assignee.
type beadHold struct {
	Status   string
	Assignee string
}

// resolveSpawnBaseBranch computes the merge-target base branch reported on
// SpawnedPolecatInfo.BaseBranch, given the caller's raw --base-branch value
// (already origin/-qualified if auto-detected) and the rig's default branch.
//
// gt-a8i3: deliberately takes no resumeBranch parameter. A resume dispatch
// (`gt sling --branch/--pr`) checks out an existing branch as the polecat's
// working branch, but that has no bearing on what the work should merge
// into — conflating the two here previously caused `gt done`/`gt mq submit`
// to submit a self-targeted MR that merges as a no-op and deletes the only
// copy of the work on cleanup.
func resolveSpawnBaseBranch(baseBranch, defaultBranch string) string {
	effectiveBranch := strings.TrimPrefix(baseBranch, "origin/")
	if effectiveBranch == "" {
		effectiveBranch = defaultBranch
	}
	return effectiveBranch
}

// AgentID returns the agent identifier (e.g., "gastown/polecats/Toast")
func (s *SpawnedPolecatInfo) AgentID() string {
	return fmt.Sprintf("%s/polecats/%s", s.RigName, s.PolecatName)
}

// SessionStarted returns true if the tmux session has been started.
func (s *SpawnedPolecatInfo) SessionStarted() bool {
	return s.Pane != ""
}

// SlingSpawnOptions contains options for spawning a polecat via sling.
type SlingSpawnOptions struct {
	TownRoot      string // Gas Town workspace root; falls back to cwd when empty
	Force         bool   // Force spawn even if polecat has uncommitted work, and past merge-queue backpressure (gt-xidg)
	Account       string // Claude Code account handle to use
	Create        bool   // Create polecat if it doesn't exist (currently always true for sling)
	HookBead      string // Bead ID to set as hook_bead at spawn time (atomic assignment)
	Agent         string // Agent override for this spawn (e.g., "claude-haiku")
	BaseBranch    string // Override base branch for polecat worktree (e.g., "develop", "release/v2")
	ResumeBranch  string // Resume an existing branch (e.g. PR head) instead of creating polecat/<name>/<bead>+<ts>
	SkipAdmission bool   // Caller already holds a polecat admission reservation
	// Name is the exact polecat a named sling targets (gt sling <bead>
	// <rig>/<name>). Set, it is reused or — with Create — created by that
	// name, or the sling is refused; the pool never substitutes another
	// polecat (gt-2w4f9). Empty lets the pool choose.
	Name string
	// Steps times the spawn path's stages (gt-llg8). Nil falls back to this
	// process's timer; the daemon passes its own so the lines reach its log
	// tagged with what it was feeding.
	Steps func(name string)
}

// step records the end of one spawn stage on the timer this spawn was handed,
// or on this process's timer when it was handed none.
func (o SlingSpawnOptions) step(name string) {
	if o.Steps != nil {
		o.Steps(name)
		return
	}
	slingSteps.Step(name)
}

func effectivePolecatDirCap(configured int) int {
	if configured < minPolecatDirsPerRig {
		return minPolecatDirsPerRig
	}
	return configured
}

// polecatIntegrationEnabled reports whether a hooked bead's parent epic
// integration branch should be auto-sourced for a spawning polecat's
// worktree, resolved across rig-root -> repo -> rig-local (gt-xwt9), the
// same precedence every gate-command call site must use. Nil-safe: an
// unresolvable config defaults to enabled, matching the pre-existing
// behavior of the two call sites this replaces.
func polecatIntegrationEnabled(townRoot, rigName string) bool {
	if mq := rig.ResolveMergeQueueConfig(townRoot, rigName); mq != nil {
		return mq.IsPolecatIntegrationEnabled()
	}
	return true
}

// brokenIdleReclaimer is the polecat-manager surface the pre-allocation
// broken-idle reclaim sweep needs.
type brokenIdleReclaimer interface {
	List() ([]*polecat.Polecat, error)
	ReclaimBrokenIdlePolecat(name string) error
}

// reclaimBrokenIdleUnlessNamed runs the pool-wide broken-idle reclaim before
// a rig sling allocates. A named sling touches no polecat but its own
// (gt-2w4f9), so it skips the sweep.
func reclaimBrokenIdleUnlessNamed(polecatMgr brokenIdleReclaimer, opts SlingSpawnOptions) {
	if opts.Name != "" {
		return
	}
	if reclaimed, err := reclaimBrokenIdlePolecatForSling(polecatMgr); err != nil {
		style.PrintWarning("could not reclaim broken idle polecat before allocation: %v", err)
	} else if reclaimed {
		fmt.Println("  Allocating fresh polecat after reclaiming broken idle sandbox...")
	}
}

func reclaimBrokenIdlePolecatForSling(polecatMgr brokenIdleReclaimer) (bool, error) {
	polecats, err := polecatMgr.List()
	if err != nil {
		return false, err
	}

	for _, candidate := range polecats {
		if candidate == nil || candidate.State != polecat.StateIdle || candidate.Issue != "" {
			continue
		}
		verifyErr := verifyWorktreeExists(candidate.ClonePath)
		if verifyErr == nil || !polecat.IsStructuralWorktreeError(verifyErr) {
			continue
		}

		fmt.Printf("  Reclaiming broken idle polecat %s before allocation: %v\n", candidate.Name, verifyErr)
		if err := polecatMgr.ReclaimBrokenIdlePolecat(candidate.Name); err != nil {
			fmt.Printf("  Broken idle polecat %s was not safe to reclaim: %v\n", candidate.Name, err)
			continue
		}
		fmt.Printf("  %s Broken idle polecat %s reclaimed before assigning new work\n", style.Bold.Render("✓"), candidate.Name)
		return true, nil
	}

	return false, nil
}

// idlePolecatReuse is the polecat-manager surface the idle-reuse path needs.
type idlePolecatReuse interface {
	FindIdlePolecat() (*polecat.Polecat, error)
	ReuseIdlePolecat(name string, opts polecat.AddOptions) (*polecat.Polecat, error)
	Get(name string) (*polecat.Polecat, error)
}

// reuseIdlePolecatForSling reuses an idle polecat's sandbox for this sling
// (gt-4ac), returning nil info when the caller should allocate a fresh polecat.
// A refusal to take a branch another worktree holds stops the sling: the
// fresh-allocation fallback attaches a worktree to that same branch by forcing
// past git's own check, which builds the two-worktrees-one-ref state the refusal
// detected (gt-0kk2). With opts.Name set, only that polecat is considered and
// any refusal stops the sling (gt-2w4f9).
//
// An unnamed sling of a rework bead prefers the polecat that built the branch
// the bead came back on, resuming that branch (gt-lid6d). That preference,
// unlike opts.Name, never stops the sling: a seat that cannot take it is
// logged and the pool chooses as usual.
func reuseIdlePolecatForSling(
	polecatMgr idlePolecatReuse,
	t *tmux.Tmux,
	r *rig.Rig,
	townRoot, rigName string,
	opts SlingSpawnOptions,
	recordRespawn func(),
) (*SpawnedPolecatInfo, error) {
	return reuseIdlePolecatForSlingWith(polecatMgr, realIdleReuseEnv(t, r, townRoot, rigName, opts.step), rigName, opts, recordRespawn)
}

// idleReuseEnv is what the idle-reuse path reads and writes besides the polecat
// manager: the integration-branch detection, the worktree check, the session
// name, the rig's default branch, the spawn feed event and the step timer.
// realIdleReuseEnv wires the real ones.
type idleReuseEnv struct {
	// integrationBranch returns the integration branch the hooked bead's epic
	// carries (without origin/), or "" when none applies.
	integrationBranch func(hookBead string) string
	verifyWorktree    func(clonePath string) error
	sessionName       func(polecatName string) string
	defaultBranch     func() string
	logSpawn          func(rigName, polecatName string)
	step              func(name string)
	// reworkSeat names the polecat a rework bead goes back to and the branch
	// its rejected attempt is on, or ok=false for any other bead (gt-lid6d).
	reworkSeat func(beadID string) (*reworkSeat, bool)
}

func realIdleReuseEnv(t *tmux.Tmux, r *rig.Rig, townRoot, rigName string, step func(name string)) idleReuseEnv {
	return idleReuseEnv{
		integrationBranch: func(hookBead string) string {
			return detectSpawnIntegrationBranch(townRoot, rigName, r, hookBead)
		},
		verifyWorktree: verifyWorktreeExists,
		sessionName:    polecat.NewSessionManager(t, r, townRegistry()).SessionName,
		defaultBranch:  r.DefaultBranch,
		logSpawn: func(rigName, polecatName string) {
			_ = events.LogFeed(events.TypeSpawn, events.ActorGt, events.SpawnPayload(rigName, polecatName))
		},
		step:       step,
		reworkSeat: func(beadID string) (*reworkSeat, bool) { return reworkSeatFor(townRoot, beadID) },
	}
}

// detectSpawnIntegrationBranch auto-detects the integration branch of the
// hooked bead's parent epic, when the rig has integration branches enabled.
// It returns "" when none applies or detection fails.
func detectSpawnIntegrationBranch(townRoot, rigName string, r *rig.Rig, hookBead string) string {
	if !polecatIntegrationEnabled(townRoot, rigName) {
		return ""
	}
	repoGit, repoErr := getRigGit(r.Path)
	if repoErr != nil {
		return ""
	}
	detected, detectErr := beads.DetectIntegrationBranch(beads.New(r.Path), repoGit, hookBead)
	if detectErr != nil {
		return ""
	}
	return detected
}

// reuseIdlePolecatForSlingWith is reuseIdlePolecatForSling with its
// collaborators explicit.
func reuseIdlePolecatForSlingWith(
	polecatMgr idlePolecatReuse,
	env idleReuseEnv,
	rigName string,
	opts SlingSpawnOptions,
	recordRespawn func(),
) (*SpawnedPolecatInfo, error) {
	polecatName := opts.Name
	heldIssue := "" // work the named polecat already holds, for the refusal hint
	resumeBranch := opts.ResumeBranch
	// allowRecovery is set for the rework preference below: the seat is being
	// resumed on the branch its rejected attempt is already on, so the reuse
	// gate's NEEDS_RECOVERY verdict for that attempt's commits is not a reason
	// to refuse (gt-lid6d).
	allowRecovery := false
	if polecatName != "" {
		// A named sling reuses that polecat or nothing (gt-2w4f9). An absent
		// one returns (nil, nil) only under --create, so the caller creates
		// it by that name.
		named, err := namedPolecatExistsForSling(polecatMgr, rigName, opts)
		if err != nil || named == nil {
			return nil, err
		}
		heldIssue = named.Issue
		fmt.Printf("Reusing named polecat: %s\n", polecatName)
	} else {
		// A rework goes back to the polecat that built its branch. The
		// preference is soft: an unavailable seat says why and the pool picks,
		// exactly as a non-rework sling does.
		if seat, ok := reworkPreference(env, opts); ok {
			if why := reworkSeatUnavailable(polecatMgr, seat, opts.HookBead); why != "" {
				fmt.Printf("  Rework %s: %s; letting the pool choose\n", opts.HookBead, why)
			} else {
				polecatName = seat.PolecatName
				resumeBranch = seat.Branch
				allowRecovery = true
				fmt.Printf("Rework %s: back to %s on %s\n", opts.HookBead, polecatName, resumeBranch)
			}
		}
	}
	if polecatName == "" {
		idlePolecat, findErr := polecatMgr.FindIdlePolecat()
		if findErr != nil || idlePolecat == nil {
			return nil, nil
		}
		polecatName = idlePolecat.Name
		fmt.Printf("Reusing idle polecat: %s\n", polecatName)
	}

	// ResumeBranch takes precedence over BaseBranch / integration auto-detection:
	// when the user (or scheduler) wants to resume an existing PR branch, we
	// must not start from main or an integration branch.
	baseBranch := opts.BaseBranch
	if resumeBranch == "" {
		if baseBranch == "" && opts.HookBead != "" {
			if detected := env.integrationBranch(opts.HookBead); detected != "" {
				baseBranch = "origin/" + detected
				fmt.Printf("  Auto-detected integration branch: %s\n", detected)
			}
		}
		if baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
			baseBranch = "origin/" + baseBranch
		}
	}

	// Reuse the idle polecat with branch-only operations (no worktree add/remove),
	// skipping ~5s of worktree creation (persistent-polecat-pool phase 3). Any
	// other failure allocates a fresh polecat rather than repairing this worktree
	// destructively.
	addOpts := polecat.AddOptions{
		HookBead:      opts.HookBead,
		BaseBranch:    baseBranch,
		ResumeBranch:  resumeBranch,
		AllowRecovery: allowRecovery,
	}
	if _, err := polecatMgr.ReuseIdlePolecat(polecatName, addOpts); err != nil {
		if opts.Name != "" {
			return nil, namedPolecatRefusal(rigName, polecatName, opts.HookBead, heldIssue, err)
		}
		// Only a resume can end up on a branch someone already holds: a fresh
		// sling names a new branch, so its fallback cannot collide (gt-0kk2).
		if errors.Is(err, polecat.ErrBranchHeld) && resumeBranch != "" {
			return nil, fmt.Errorf("cannot reuse idle polecat %s: %w", polecatName, err)
		}
		if allowRecovery {
			fmt.Printf("  Rework %s: could not resume %s on %s (%v); letting the pool choose\n",
				opts.HookBead, polecatName, resumeBranch, err)
		} else if errors.Is(err, polecat.ErrPolecatNeedsRecovery) {
			fmt.Printf("  Idle polecat %s needs recovery before reuse: %v; allocating new...\n", polecatName, err)
		} else {
			fmt.Printf("  Branch-only reuse failed for idle polecat %s: %v; allocating new...\n", polecatName, err)
		}
		return nil, nil
	}

	polecatObj, err := polecatMgr.Get(polecatName)
	if err != nil {
		return nil, fmt.Errorf("getting idle polecat after reuse: %w", err)
	}
	if err := env.verifyWorktree(polecatObj.ClonePath); err != nil {
		return nil, fmt.Errorf("worktree verification failed for reused %s: %w", polecatName, err)
	}

	sessionName := env.sessionName(polecatName)

	fmt.Printf("%s Polecat %s reused (idle → working, session start deferred)\n", style.Bold.Render("✓"), polecatName)
	env.step("reuse")
	env.logSpawn(rigName, polecatName)
	recordRespawn()

	return &SpawnedPolecatInfo{
		RigName:     rigName,
		PolecatName: polecatName,
		ClonePath:   polecatObj.ClonePath,
		SessionName: sessionName,
		Pane:        "",
		BaseBranch:  resolveSpawnBaseBranch(baseBranch, env.defaultBranch()),
		Branch:      polecatObj.Branch,
		// A reused sandbox predates this sling: rollback keeps it. Its new
		// branch stays checked out there, so rollback leaves that too. A
		// rework's branch predates the sling too (it is the rejected
		// attempt), so resumeBranch — not opts.ResumeBranch — is what says
		// whether this sling created the branch.
		FreshSpawn:    false,
		BranchCreated: resumeBranch == "",
		HookBead:      opts.HookBead,
		account:       opts.Account,
		agent:         opts.Agent,
	}, nil
}

// reworkPreference is the seat an unnamed sling of a rework bead should try
// first (gt-lid6d): the polecat its latest submission names, and the branch
// that submission is on. No preference is taken for a named sling — it names
// its own seat — nor for a sling that already names a branch to resume, nor
// for a bead that is not rework or names no polecat.
func reworkPreference(env idleReuseEnv, opts SlingSpawnOptions) (*reworkSeat, bool) {
	if opts.Name != "" || opts.ResumeBranch != "" || opts.HookBead == "" || env.reworkSeat == nil {
		return nil, false
	}
	return env.reworkSeat(opts.HookBead)
}

// reworkSeatUnavailable explains why the polecat a rework came back from
// cannot take the sling, or "" when it can. Only a seat that is standing on
// the branch being resumed, is not working and holds no hook for another bead
// qualifies: a rework must never displace work that is in flight. The bead
// being slung is not another bead — it is that seat's own rejected work — so
// a seat still hooked to it may take the sling, which is how the reuse gate
// sees it too. The branch test is what tells this seat from one another sling
// has already started on, which has no hook to read yet (reworkResumeAllowed
// re-checks all three under the reuse lock).
func reworkSeatUnavailable(polecatMgr idlePolecatReuse, seat *reworkSeat, hookBead string) string {
	p, err := polecatMgr.Get(seat.PolecatName)
	switch {
	case errors.Is(err, polecat.ErrPolecatNotFound):
		return "polecat " + seat.PolecatName + " no longer exists"
	case err != nil:
		return fmt.Sprintf("could not read polecat %s: %v", seat.PolecatName, err)
	case p == nil:
		return "polecat " + seat.PolecatName + " could not be read"
	}
	if p.Branch != seat.Branch {
		return fmt.Sprintf("polecat %s is not on %s", seat.PolecatName, seat.Branch)
	}
	if p.State == polecat.StateWorking {
		return "polecat " + seat.PolecatName + " is working"
	}
	if p.Issue != "" && p.Issue != hookBead {
		return fmt.Sprintf("polecat %s holds %s", seat.PolecatName, p.Issue)
	}
	return ""
}

// namedPolecatExistsForSling returns the polecat a named sling targets, or
// nil when it does not exist (gt-2w4f9). A missing polecat is acceptable only
// with --create; a lookup that fails refuses, because guessing is how a named
// sling ends up on another polecat.
func namedPolecatExistsForSling(polecatMgr idlePolecatReuse, rigName string, opts SlingSpawnOptions) (*polecat.Polecat, error) {
	p, err := polecatMgr.Get(opts.Name)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, polecat.ErrPolecatNotFound):
		if opts.Create {
			return nil, nil
		}
		return nil, namedPolecatMissingRefusal(rigName, opts.Name, opts.HookBead)
	default:
		return nil, fmt.Errorf("reading named polecat %s/%s: %w", rigName, opts.Name, err)
	}
}

// namedPolecatMissingRefusal is the refusal for a named polecat that does not
// exist without --create. The live lookup and the dry-run peek both build it
// here, so a preview prints the refusal the sling would raise (gt-yxc7m).
func namedPolecatMissingRefusal(rigName, name, hookBead string) error {
	return fmt.Errorf("polecat %s/%s does not exist; not substituting another polecat\n"+
		"Create it by that name: add --create\n"+
		"Let the pool choose:    gt sling %s %s",
		rigName, name, beadOrPlaceholder(hookBead), rigName)
}

// peekNamedPolecatSling is the named-polecat reuse decision without its side
// effects — the named counterpart of peekPolecatPoolAgent: a preview must
// print the route a live sling would take, a refusal included, without reusing
// anything (detach, reset --hard, clean -f, a new branch, hook_bead) and
// without creating anything (gt-yxc7m). Nothing is admitted, so no seat is
// claimed. The refusal text is the one the live path builds, hint included.
func peekNamedPolecatSling(townRoot, rigName string, opts SlingSpawnOptions) error {
	_, _, polecatMgr, err := openSlingRig(townRoot, rigName)
	if err != nil {
		// The rig is unreadable. A live sling reports that itself; a preview
		// says nothing rather than inventing a refusal it cannot know.
		return nil
	}
	heldIssue, peekErr := polecatMgr.PeekNamedReuse(opts.Name)
	return namedSlingRefusal(rigName, opts, heldIssue, peekErr)
}

// namedSlingRefusal is the refusal a live named sling would raise, given the
// read-only reuse verdict: nil when the route stands. Separated from the rig
// lookup so the mapping is testable without a rig.
func namedSlingRefusal(rigName string, opts SlingSpawnOptions, heldIssue string, err error) error {
	switch {
	case errors.Is(err, polecat.ErrPolecatNotFound):
		if opts.Create {
			return nil // --create would make it under this name
		}
		return namedPolecatMissingRefusal(rigName, opts.Name, opts.HookBead)
	case err != nil:
		return namedPolecatRefusal(rigName, opts.Name, opts.HookBead, heldIssue, err)
	}
	return nil
}

// namedPolecatRefusal explains why a named polecat cannot take this sling.
// The sling stops here: falling back to the pool is the silent substitution
// gt-2w4f9 forbids. The hint depends on the cause, and never suggests hooking
// the new bead onto a polecat that holds other work:
//   - parked: resume the park, then retry the same sling;
//   - already holding the bead being slung: resume that session;
//   - anything else (busy, other or unpushed work, broken worktree): inspect
//     it with check-recovery, or let the pool choose.
func namedPolecatRefusal(rigName, name, hookBead, heldIssue string, err error) error {
	bead := beadOrPlaceholder(hookBead)
	addr := rigName + "/" + name
	var hint string
	switch {
	case errors.Is(err, polecat.ErrPolecatParked):
		// Retry with the full <rig>/polecats/<name> form: the <rig>/<name>
		// shorthand only reaches a sessionless polecat under --create.
		hint = fmt.Sprintf("Resume the park, then retry: gt agent resume %s && gt sling %s %s/polecats/%s", addr, bead, rigName, name)
	case hookBead != "" && heldIssue == hookBead:
		hint = fmt.Sprintf("It already holds %s; resume that work: gt session start %s --issue %s", hookBead, addr, hookBead)
	default:
		if heldIssue != "" {
			hint = fmt.Sprintf("It holds %s. ", heldIssue)
		}
		hint += fmt.Sprintf("Inspect it: gt polecat check-recovery %s", addr)
	}
	return fmt.Errorf("named polecat %s cannot take this sling: %w\n"+
		"Not substituting another polecat.\n"+
		"%s\n"+
		"Let the pool choose: gt sling %s %s",
		addr, err, hint, bead, rigName)
}

func beadOrPlaceholder(bead string) string {
	if bead == "" {
		return "<bead>"
	}
	return bead
}

// polecatAllocator is the polecat-manager surface that creates a new polecat.
type polecatAllocator interface {
	AllocateAndAdd(opts polecat.AddOptions) (string, *polecat.Polecat, error)
	AddNamedWithOptions(name string, opts polecat.AddOptions) (*polecat.Polecat, error)
}

// allocatePolecatForSling creates the polecat a sling will use. The pool picks
// the name unless the sling named one, which is created by exactly that name
// or refused (gt-2w4f9). Both paths reserve the name under the pool lock
// before building the worktree (AllocateAndAdd / AddNamedWithOptions), so a
// concurrent allocation cannot be handed it; a name another process already
// holds is ErrPolecatExists.
func allocatePolecatForSling(polecatMgr polecatAllocator, rigName, name string, addOpts polecat.AddOptions) (string, error) {
	if name == "" {
		allocated, _, err := polecatMgr.AllocateAndAdd(addOpts)
		return allocated, err
	}
	if _, err := polecatMgr.AddNamedWithOptions(name, addOpts); err != nil {
		return "", fmt.Errorf("creating named polecat %s/%s: %w", rigName, name, err)
	}
	return name, nil
}

// SpawnPolecatForSling prepares a polecat for a sling, session start deferred.
// For a rig target (opts.Name empty) it reuses an idle polecat from the pool
// or allocates a fresh one. For a named target (gt sling <bead>
// <rig>/<name> with no live session) it reuses exactly that polecat, creates
// it by that name under --create, or refuses (gt-2w4f9).
// The caller (sling) handles hook attachment, session start and nudging.
func SpawnPolecatForSling(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	// Find workspace
	townRoot := opts.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = workspace.FindFromCwdOrError()
		if err != nil {
			return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
	}
	spawnInfo, err := realSlingSeatSpawn().spawn(townRoot, rigName, opts)
	if err != nil {
		return nil, err
	}
	// Record the town on the spawn: the session is started later and, on the
	// daemon's in-process path, from a process whose cwd is not this town.
	spawnInfo.townRoot = townRoot
	return spawnInfo, nil
}

// slingSeatSpawn is the front of SpawnPolecatForSling: the rig's backpressure,
// the pool's seat decision, and the ownership of the seat that decision
// claimed. prepare is everything after the seat is chosen — the rig, the
// polecat and its worktree. realSlingSeatSpawn wires the real ones.
type slingSeatSpawn struct {
	backpressure func(townRoot, rigName string, opts SlingSpawnOptions) error
	// resolvePool is the pool decision for this spawn, claiming a seat in
	// seatClaims.
	resolvePool func(townRoot, requested string) (agent, reason string, err error)
	releaseSeat func()
	prepare     func(townRoot, rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error)
	// seatClaims is the store this spawn's pool decision claims in and
	// releaseSeat drops from. It is handed to the spawn record on success, so
	// StartSession and the rollback drop the same claim this spawn made.
	seatClaims *poolSeatClaimStore
}

func realSlingSeatSpawn() slingSeatSpawn {
	// One store per spawn, never one per process: the daemon dispatches
	// concurrently inside a single process, and a shared store would let one
	// dispatch's release drop a seat another is still standing on (gt-t8q5).
	store := newPoolSeatClaimStore()
	return slingSeatSpawn{
		backpressure: checkSlingBackpressure,
		resolvePool: func(townRoot, requested string) (agent, reason string, err error) {
			return poolRouterFor(townRoot, store).route(requested, true)
		},
		releaseSeat: store.release,
		prepare:     prepareSlingPolecat,
		seatClaims:  store,
	}
}

// spawn is SpawnPolecatForSling once the town root is known.
func (s slingSeatSpawn) spawn(townRoot, rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	// Pre-dispatch backpressure (gt-xidg, plan Task 3 / A3): the rig's landing
	// queue is the limit on what the town can absorb, so a sling is refused
	// while the rig has more beads waiting to land than
	// merge_queue.max_ready_for_dispatch allows. It runs before the pool decision below, which costs a tmux round
	// trip and may claim a seat for a polecat that will never spawn.
	if err := s.backpressure(townRoot, rigName, opts); err != nil {
		return nil, err
	}

	// Polecat seat pool: the town's polecat_pool caps live polecats on its
	// agent (see sling_pool.go). It is consulted on every spawn path, --agent
	// included: an agent that names the pool's seat is admitted by its cap or
	// refused, so a requested agent can no longer spawn
	// past a full pool (gt-4lbz). An agent the pool does not own leaves it with
	// no opinion and the request stands. The reason line always names the agent
	// the pool chose, and a pool whose seat is at its cap refuses the sling.
	poolAgent, poolReason, poolErr := s.resolvePool(townRoot, opts.Agent)
	if poolErr != nil {
		return nil, poolErr
	}
	if poolReason != "" {
		fmt.Printf("%s %s\n", style.Dim.Render("→"), poolReason)
	}
	// Only a seat the pool named replaces the request: an empty agent is the
	// pool saying it has no opinion, not one saying "the role default".
	if poolAgent != "" {
		opts.Agent = poolAgent
	}

	// The seat claimed above belongs to the polecat this call is about to
	// spawn: the SpawnedPolecatInfo returned below carries its store to
	// StartSession, which drops it once the tmux session exists — the session
	// is what the pool counts from then on. Every other way out of this function leaves no
	// session behind — a rig that will not load, Dolt down, no connection
	// capacity, a parked rig, the respawn breaker, the per-rig directory cap, a
	// failed allocation — so the claim is dropped on the way out. A claim a
	// failed spawn kept stands for a polecat that never existed: every other
	// sling counts it as a seat taken, and cleanup leaves it alone for
	// poolSeatClaimTTL (30m) because the process holding it is still alive
	// (gt-t8q5). The spawn succeeds only by returning the polecat whose
	// session the caller will start, so success is the hand-over.
	info, err := s.prepare(townRoot, rigName, opts)
	if err != nil {
		s.releaseSeat()
		return nil, err
	}
	info.seatClaim = s.seatClaims
	return info, nil
}

// prepareSlingPolecat is the part of SpawnPolecatForSling after the pool chose
// the seat: load the rig, reuse or allocate the polecat, and build its
// worktree. Every error return leaves no session behind, so its caller drops
// the seat claim on any error.
// openSlingRig opens a rig and the polecat manager a sling reads it through.
// The spawn path and the --dry-run peek share it, so a preview inspects
// exactly the polecats a live spawn would.
func openSlingRig(townRoot, rigName string) (*rig.Rig, *tmux.Tmux, *polecat.Manager, error) {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfigOrEmpty(rigsConfigPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading rigs config: %w", err)
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rig '%s' not found", rigName)
	}

	// The polecat manager carries tmux for session-aware allocation.
	polecatGit := git.NewGit(r.Path)
	t := tmux.NewTmux()
	return r, t, supervisedPolecatManager(r, polecatGit, t, operatorActor("gt sling")), nil
}

func prepareSlingPolecat(townRoot, rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	r, t, polecatMgr, err := openSlingRig(townRoot, rigName)
	if err != nil {
		return nil, err
	}

	// Pre-spawn Dolt health check (gt-94llt7): verify Dolt is reachable before
	// allocating a polecat. Prevents orphaned polecats when Dolt is down.
	if err := polecatMgr.CheckDoltHealth(); err != nil {
		return nil, fmt.Errorf("pre-spawn health check failed: %w", err)
	}

	// Pre-spawn admission control (gt-1obzke): verify Dolt server has connection
	// capacity before spawning. Prevents connection storms during mass sling.
	if err := polecatMgr.CheckDoltServerCapacity(); err != nil {
		return nil, fmt.Errorf("admission control: %w", err)
	}

	if err := slingBlocked(townRoot, rigName, estop.ActiveFor, IsRigParkedOrDocked); err != nil {
		return nil, err
	}

	// The reclaim sweep frees a slot that counts toward capacity, so it runs
	// before admission: after it, a rig whose slots are all held by broken
	// sandboxes refuses the spawn, and the refusal stops the sweep that would
	// free one (gt-1kbi).
	reclaimBrokenIdleUnlessNamed(polecatMgr, opts)

	var admission *polecatAdmissionHandle
	if !opts.SkipAdmission {
		admission, _, err = acquirePolecatAdmissionFn(townRoot, rigName, opts.HookBead, "spawn-or-reuse")
		if err != nil {
			return nil, err
		}
		defer admission.Release()
	}
	opts.step("admission")

	// Per-bead respawn circuit breaker (clown show #22):
	// Track how many times this bead has been slung. Block after N attempts
	// to prevent witness→deacon→sling feedback loops.
	//
	// The check runs here, before anything is allocated, but the count is
	// recorded by recordRespawn at each success below. Counting a sling that
	// never reached a polecat spent the bead's budget on a process someone
	// killed: two daemon re-feeds the operator shot down mid-sling took gt-0vh
	// to its respawn limit with only one polecat ever attached (gt-4lbz).
	//
	// A count that cannot be recorded is reported, not rolled back: the polecat
	// is already attached, and ShouldBlockRespawn has already refused the sling
	// if the state file or its lock is unreadable (gt-u3hc1).
	recordRespawn := func() {
		if opts.HookBead != "" && !opts.Force {
			if _, err := polecat.RecordBeadRespawn(townRoot, opts.HookBead); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: recording respawn of %s: %v\n", opts.HookBead, err)
			}
		}
	}
	if opts.HookBead != "" && !opts.Force {
		if polecat.ShouldBlockRespawn(townRoot, opts.HookBead) {
			maxRespawns := config.LoadOperationalConfig(townRoot).GetRecoveryConfig().MaxBeadRespawnsV()
			return nil, fmt.Errorf("respawn limit reached for %s (%d attempts). "+
				"This bead keeps failing — investigate before re-dispatching.\n"+
				"Override: gt sling %s %s --force\n"+
				"Reset:    gt sling respawn-reset %s",
				opts.HookBead, maxRespawns,
				opts.HookBead, rigName, opts.HookBead)
		}
	}

	// Persistent polecat model (gt-4ac): reuse an idle polecat's sandbox before
	// paying for a new worktree.
	reusedIdle, err := reuseIdlePolecatForSling(polecatMgr, t, r, townRoot, rigName, opts, recordRespawn)
	if err != nil {
		return nil, err
	}
	if reusedIdle != nil {
		// A reused sandbox still has its session started by the caller, which
		// is where the claim stops standing for a seat.
		return reusedIdle, nil
	}

	// Per-rig directory cap: prevent unbounded worktree accumulation, but only
	// after trying safe reuse. A reusable preserved polecat should not be blocked
	// just because the rig is already at the directory cap.
	maxPolecatDirsPerRig := effectivePolecatDirCap(r.GetIntConfig("max_polecats"))
	rigPolecatDir := filepath.Join(townRoot, rigName, "polecats")
	if entries, err := os.ReadDir(rigPolecatDir); err == nil {
		dirCount := 0
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				dirCount++
			}
		}
		if dirCount >= maxPolecatDirsPerRig {
			return nil, fmt.Errorf("rig %s has %d polecat directories (max %d). "+
				"Resolve recovery-needed polecats before allocating more slots: gt polecat list %s",
				rigName, dirCount, maxPolecatDirsPerRig, rigName)
		}
	}

	// Determine base branch for polecat worktree.
	// ResumeBranch (gh#3602) takes precedence: when resuming an existing branch
	// we must not start from main or auto-detect an integration branch.
	baseBranch := opts.BaseBranch
	if opts.ResumeBranch == "" {
		if baseBranch == "" && opts.HookBead != "" {
			// Auto-detect: check if the hooked bead's parent epic has an integration branch
			if detected := detectSpawnIntegrationBranch(townRoot, rigName, r, opts.HookBead); detected != "" {
				baseBranch = "origin/" + detected
				fmt.Printf("  Auto-detected integration branch: %s\n", detected)
			}
		}
		if baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
			baseBranch = "origin/" + baseBranch
		}
	}

	// Build add options with hook_bead set atomically at spawn time
	addOpts := polecat.AddOptions{
		HookBead:     opts.HookBead,
		BaseBranch:   baseBranch,
		ResumeBranch: opts.ResumeBranch,
	}

	// Nothing to reuse — create the polecat. The pool path (AllocateAndAdd,
	// GH#2215) and the named path (AddNamedWithOptions, gt-2w4f9) both hold the
	// pool lock through directory creation, so no concurrent process can be
	// handed the same name.
	polecatName, err := allocatePolecatForSling(polecatMgr, rigName, opts.Name, addOpts)
	if err != nil {
		return nil, fmt.Errorf("allocating and creating polecat: %w", err)
	}
	fmt.Printf("Created polecat: %s\n", polecatName)
	opts.step("allocate")

	// Get polecat object for path info
	polecatObj, err := polecatMgr.Get(polecatName)
	if err != nil {
		return nil, fmt.Errorf("getting polecat after creation: %w", err)
	}

	// Verify worktree was actually created (fixes #1070)
	// The identity bead may exist but worktree creation can fail silently
	if err := verifyWorktreeExists(polecatObj.ClonePath); err != nil {
		// Clean up the partial state before returning error
		_ = polecatMgr.Remove(polecatName, true) // force=true to clean up partial state
		return nil, fmt.Errorf("worktree verification failed for %s: %w\nHint: try 'gt polecat nuke %s/%s --force' to clean up",
			polecatName, err, rigName, polecatName)
	}

	// Get session manager for session name (session start is deferred)
	polecatSessMgr := polecat.NewSessionManager(t, r, townRegistry())
	sessionName := polecatSessMgr.SessionName(polecatName)

	opts.step("worktree")
	fmt.Printf("%s Polecat %s spawned (session start deferred)\n", style.Bold.Render("✓"), polecatName)

	// Log spawn event to activity feed
	_ = events.LogFeed(events.TypeSpawn, events.ActorGt, events.SpawnPayload(rigName, polecatName))
	recordRespawn()

	effectiveBranch := resolveSpawnBaseBranch(baseBranch, r.DefaultBranch())

	// The spawn is real: the claim now belongs to a polecat whose session the
	// caller will start, and StartSession is what drops it.
	return &SpawnedPolecatInfo{
		RigName:     rigName,
		PolecatName: polecatName,
		ClonePath:   polecatObj.ClonePath,
		SessionName: sessionName,
		Pane:        "", // Empty until StartSession is called
		BaseBranch:  effectiveBranch,
		Branch:      polecatObj.Branch,
		FreshSpawn:  true,
		// A resume dispatch checks out a branch that already existed.
		BranchCreated: opts.ResumeBranch == "",
		HookBead:      opts.HookBead,
		account:       opts.Account,
		agent:         opts.Agent,
	}, nil
}

// StartSession starts the tmux session for a spawned polecat.
// This is called after the molecule/bead is attached, so the polecat
// sees its work when gt prime runs on session start.
// Returns the pane ID after session start.
func (s *SpawnedPolecatInfo) StartSession() (string, error) {
	pane, err := s.startSession()
	s.noteStartOutcome(err)
	return pane, err
}

// noteStartOutcome records a failed session start against the bead, and
// clears the record on a successful one. A sling that fails here leaves the
// bead open and unassigned — the state automatic dispatch draws from — so
// without the record it is re-slung on the next tick (gt-wacl).
func (s *SpawnedPolecatInfo) noteStartOutcome(startErr error) {
	if s.HookBead == "" {
		return
	}
	townRoot, err := s.town()
	if err != nil {
		return
	}
	s.noteStartOutcomeIn(townRoot, startErr)
}

// town is the town this spawn belongs to: the one its spawner recorded, or the
// cwd's when the record has none.
func (s *SpawnedPolecatInfo) town() (string, error) {
	if s.townRoot != "" {
		return s.townRoot, nil
	}
	return workspace.FindFromCwdOrError()
}

// noteStartOutcomeIn is noteStartOutcome for the town at townRoot.
func (s *SpawnedPolecatInfo) noteStartOutcomeIn(townRoot string, startErr error) {
	if s.HookBead == "" {
		return
	}
	if startErr == nil {
		dispatch.ClearStartupFailure(townRoot, s.HookBead)
		return
	}
	if err := dispatch.RecordStartupFailure(townRoot, s.HookBead, startErr.Error()); err != nil {
		style.PrintWarning("could not record the startup failure for %s: %v", s.HookBead, err)
	}
}

func (s *SpawnedPolecatInfo) startSession() (string, error) {
	// The tmux session this starts is what the pool counts, so the seat claim
	// this spawn made for it (sling_pool.go) is redundant the moment the
	// session exists — and holding both would read one polecat as two seats.
	// Releasing on the way out also covers the failure paths, where no session
	// will ever appear and the seat must not stay claimed.
	defer s.releaseSeatClaim()

	if s.SessionStarted() {
		return s.Pane, nil
	}

	townRoot, err := s.town()
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rig config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfigOrEmpty(rigsConfigPath)
	if err != nil {
		return "", fmt.Errorf("loading rigs config: %w", err)
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(s.RigName)
	if err != nil {
		return "", fmt.Errorf("rig '%s' not found", s.RigName)
	}

	// Resolve account
	accountsPath := constants.MayorAccountsPath(townRoot)
	claudeConfigDir, _, err := config.ResolveAccountConfigDir(accountsPath, s.account)
	if err != nil {
		return "", fmt.Errorf("resolving account: %w", err)
	}

	// New work for the seat ends a submitted state left by its last gt done
	// (gt-obbx2); a stale one would make the supervisor refuse every restart
	// of this dispatch. Warn-only: the label on the bead is authoritative.
	if err := intent.ClearSubmitted(townRoot, supervisor.IntentSeat(supervisor.SeatFor(s.RigName, constants.RolePolecat, s.PolecatName)), "gt sling", time.Now()); err != nil {
		style.PrintWarning("could not clear the submitted intent for %s/%s: %v", s.RigName, s.PolecatName, err)
	}

	// Start session
	t := tmux.NewTmux()
	// A start over a dead session is a Respawn and a failed startup's kill a
	// Cleanup, both through the supervisor (gt-4k3fj.4.1).
	sup := operatorSupervisor(filepath.Dir(r.Path))
	actor := operatorActor("gt sling")
	polecatSessMgr := polecat.NewSessionManager(t, r, townRegistry())
	polecatSessMgr.SetHooks(polecatSessionHooks(sup, townRegistry(), s.RigName, "sling", actor))

	fmt.Printf("Starting session for %s/%s...\n", s.RigName, s.PolecatName)
	startOpts := polecat.SessionStartOptions{
		RuntimeConfigDir: claudeConfigDir,
		Agent:            s.agent,
	}
	if err := polecatSessMgr.Start(s.PolecatName, startOpts); err != nil {
		return "", fmt.Errorf("starting session: %w", err)
	}

	// Wait for runtime to be fully ready before returning.
	// When an agent override is specified (e.g., --agent claude-haiku), resolve
	// the runtime config from the override so WaitForRuntimeReady uses that
	// agent's readiness settings rather than the role default's (gt-1j3m).
	spawnTownRoot := filepath.Dir(r.Path)
	var runtimeConfig *config.RuntimeConfig
	if s.agent != "" {
		rc, _, err := config.ResolveAgentConfigWithOverride(spawnTownRoot, r.Path, s.agent)
		if err != nil {
			style.PrintWarning("resolving agent config for %s: %v (using default)", s.agent, err)
			runtimeConfig = config.ResolveRoleAgentConfig("polecat", spawnTownRoot, r.Path)
		} else {
			runtimeConfig = rc
		}
	} else {
		runtimeConfig = config.ResolveRoleAgentConfig("polecat", spawnTownRoot, r.Path)
	}
	if err := t.WaitForRuntimeReady(s.SessionName, runtimeConfig, 30*time.Second); err != nil {
		style.PrintWarning("runtime may not be fully ready: %v", err)
	}

	// Update agent state with retry logic (gt-94llt7: fail-safe Dolt writes).
	// Note: warn-only, not fail-hard. The tmux session is already started above,
	// so returning an error here would leave an orphaned session with no cleanup path.
	// The polecat can still function without the agent state update — it only affects
	// monitoring visibility, not correctness. Compare with createAgentBeadWithRetry
	// which fails hard because a polecat without an agent bead is untrackable.
	polecatGit := git.NewGit(r.Path)
	polecatMgr := polecat.NewManager(r, polecatGit, t, townRegistry())
	if err := polecatMgr.SetAgentStateWithRetry(s.PolecatName, "working"); err != nil {
		style.PrintWarning("could not update agent state after retries: %v", err)
	}

	// Update issue status from hooked to in_progress.
	// Also warn-only for the same reason: session is already running.
	if err := polecatMgr.SetState(s.PolecatName, polecat.StateWorking); err != nil {
		style.PrintWarning("could not update issue status to in_progress: %v", err)
	}

	// Get pane — if this fails, the session may have died during startup.
	// Kill the dead session to prevent "session already running" on next attempt (gt-jn40ft).
	pane, err := getSessionPane(s.SessionName)
	if err != nil {
		// Session likely died — clean up the tmux session so it doesn't block
		// re-sling. A rollback of a session this spawn created: the
		// supervisor's Cleanup, logged and never refused.
		_ = sup.Cleanup(supervisor.SeatIn(townRegistry(), s.RigName, constants.RolePolecat, s.PolecatName),
			"sling: session died during startup", actor)
		return "", fmt.Errorf("getting pane for %s (session likely died during startup): %w", s.SessionName, err)
	}

	s.Pane = pane
	return pane, nil
}

// IsRigName checks if a target string is a rig name (not a role or path).
// Returns the rig name and true if it's a valid rig.
func IsRigName(target string) (string, bool) {
	// If it contains a slash, it's a path format (rig/role or rig/crew/name)
	if strings.Contains(target, "/") {
		return "", false
	}

	// Check known non-rig role names
	switch strings.ToLower(target) {
	case constants.RoleCrew, "ref":
		return "", false
	}

	// Try to load as a rig
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", false
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return "", false
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	_, err = rigMgr.GetRig(target)
	if err != nil {
		return "", false
	}

	return target, true
}

// verifyWorktreeExists checks that a git worktree was actually created at the given path
// and that it is a functional git repository. Returns an error if the worktree is missing,
// has a broken .git reference, or fails basic git validation. (GH#2056)
func verifyWorktreeExists(clonePath string) error {
	return polecat.VerifyWorktreeExists(clonePath)
}

// getRigGit returns a Git object for the rig's repository.
// Prefers .repo.git (bare repo) if it exists, falls back to mayor/rig.
func getRigGit(rigPath string) (*git.Git, error) {
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if info, err := os.Stat(bareRepoPath); err == nil && info.IsDir() {
		return git.NewGitWithDir(bareRepoPath, ""), nil
	}
	mayorPath := filepath.Join(rigPath, "mayor", "rig")
	if _, err := os.Stat(mayorPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no repo base found (neither .repo.git nor mayor/rig exists)")
	}
	return git.NewGit(mayorPath), nil
}
