> Status: proposed (2026-09-22). Design only, not yet implemented. Tracked in gt-glfh.

# done -> idle promotion after witness verification

Date: 2026-09-22, revised 2026-09-23 (rework: see "Revision history"). Bead:
gt-glfh. Design only — no implementation in this pass (mayor's instruction on
gt-glfh: "Do not implement without a reviewed spec").

## Decision

Wire a single, narrow promotion path: a polecat sitting in `agent_state=done`
is moved to `agent_state=idle` by the witness, and only by the witness,
once four bookkeeping facts on its agent bead line up AND its current
branch is verified at source (by patch-id, not `--is-ancestor` alone) to be
already landed on the rig's default branch. Any one of the checks failing,
erroring, or being unmeasurable leaves the polecat in `done`. `done` stays
fully reuse-eligible throughout (gt-uu6) — this design changes when a
polecat is *labeled* idle, never whether it can be reused.

This reverses gt-ho4f's original call (accept `done` as the permanent
resting state, delete `TransitionPolecatToIdle` as dead code) for the reason
gt-ho4f itself gave for not wiring it then: the risk was touching a
verification instrument that had already needed three corrective passes
that cycle (gt-7kr, gt-14a, gt-hsg). This design does not touch that
instrument. It adds an entirely separate, new code path that does not read
or write `internal/polecat/workstate.go`, `RecordedCleanupBlocks`,
`getGitStateWithTargets`, or any of the reuse/dirt-check machinery gt-ui2x
just finished hardening. It reuses only the patch-id verification primitive
that already exists for a different consumer (the aa-apw zombie-restart
skip).

**Promotion does put a polecat on paths it was not reachable from before**
(see "Consumers that treat idle differently from done" below) — the earlier
draft of this design claimed the opposite ("not on any destructive path"),
which was wrong and is corrected here.

## Consumers that treat idle differently from done

Four call sites branch on `agent_state`/polecat `State` being `idle`
specifically, not `done`. Promotion makes previously-`done` polecats reach
each of them for the first time. Each is listed with what changes and why
that is either inert or intentional and safe:

1. **`internal/polecat/reclaim.go` `brokenIdleReclaimAgentBlocker`, gated
   through `internal/polecat/manager.go:1395`
   (`ReclaimBrokenIdlePolecat`).** `ReclaimBrokenIdlePolecat` only runs when
   `current.State == StateIdle` AND `VerifyWorktreeExists` already proves the
   worktree is *structurally missing or damaged* (manager.go:1397-1401) —
   it never considers a healthy sandbox. Today a `done` polecat whose
   worktree becomes structurally broken has no path back to a clean
   bookkeeping state; it is stuck reporting `done` forever with a sandbox
   that no longer exists. After this design, if that same polecat also
   happens to pass promotion's gates (branch verified landed, `hook_bead`
   empty, `active_mr` empty, `cleanup_status == clean`), it becomes `idle`
   and `ReclaimBrokenIdlePolecat` can retire the dead bookkeeping through its
   normal non-force removal path (`removeWithOptionsLocked(name, false,
   false, false)` — the same call an operator-triggered reclaim would make).
   This is a new but intentional consequence: self-healing an orphaned,
   already-broken `done` seat instead of leaving it wedged. It is bounded by
   the structural-damage precondition, so a polecat with a healthy worktree
   is never touched by this path regardless of `agent_state`.

   The clearing of `PushFailed`/`MRFailed` on promotion (see New code §1)
   interacts directly with this consumer: `brokenIdleReclaimAgentBlocker`
   treats both flags as unconditional blockers (reclaim.go:58-63). Promotion
   already requires `cleanup_status == clean` (the blocker's other
   unconditional-except-structural-absence check), so the only scenario
   where clearing these flags changes `ReclaimBrokenIdlePolecat`'s outcome is
   a polecat that is clean, verified-landed by patch-id, and has a
   structurally broken worktree, with a stale failure flag from an earlier
   attempt. In that scenario the patch-id verification is strictly stronger,
   current evidence than the stale flag, so allowing reclaim to proceed is
   correct rather than a regression.

2. **`internal/cmd/polecat.go:1909` (`cleanupStatusReconcileCandidate`).**
   Requires `p.State == polecat.StateIdle && fields.AgentState ==
   beads.AgentStateIdle` before it will rewrite an already-non-clean
   `cleanup_status` back to `clean`. Promotion's own gate requires
   `cleanup_status == clean` already, so a freshly promoted polecat can never
   be a candidate for this reconcile at the moment of promotion — it only
   becomes reachable later if `cleanup_status` drifts dirty again after
   promotion, the same as any other idle polecat. No special interaction.

3. **`internal/witness/handlers.go:1741`, the idle-dirty-sandbox zombie
   report** (`beads.AgentState(agentState) == AgentStateIdle` branch inside
   the session-alive check). Reports (does not act on) a dirty
   `cleanup_status` for an idle polecat with a live session. Promotion
   requires `cleanup_status == clean` at the moment of the write, so a
   just-promoted polecat starts clean and cannot trigger this report until
   something dirties it afterward — identical to the existing behavior for
   any polecat that goes `spawning -> ... -> idle` today. No special
   interaction.

4. **`internal/daemon/daemon.go:3157`, the crash-detection terminal-state
   guard.** Skips crash classification when `agentState ==
   beads.AgentStateDone || agentState == beads.AgentStateNuked`. This check
   is only reached after an earlier guard (`daemon.go:3141`) returns early
   whenever `info.HookBead == ""`. Promotion only fires when `HookBead ==
   ""` already (one of its own gates), so a polecat eligible for promotion
   never reaches line 3157 in the first place, whether it is `done` or
   `idle` at the time. No special interaction — this consumer is unaffected
   by construction, not by a case this design added.

No other reader in the codebase branches on `idle` vs. `done` for a polecat
that also matches this design's promotion gates (`FindIdlePolecat`,
`IsReuseEligible`, `DecideWorkstate` already treat `done` as reuse-eligible
per gt-uu6 and are unchanged — see Non-goals).

## Why "done" needed a promotion path at all

`done` today means two different things depending on when you look: freshly
completed and not yet known to have landed, or completed and durably landed
weeks ago. Patrol already special-cases `idle` for cheap skips
(`internal/witness/handlers.go:2139`, `isSubmittedStillRunningCandidate`, and
the zombie-active-work classification at `isZombieState`) precisely because
that distinction is supposed to exist. Without a promotion path, every
patrol cycle re-examines every completed polecat forever, and there is no
state a human or dashboard can read as "verified done, not just claimed
done." This is bookkeeping/observability, not a reuse-safety fix — gt-uu6
already covers reuse safety.

## Existing primitives this design reuses (nothing new at the git layer)

**Patch-id verification already exists and is already fail-closed.**
`internal/witness/handlers.go:1523` (`_verifyBranchAlreadyMerged`, exposed as
the package var `verifyBranchAlreadyMerged` for test injection) checks
whether a polecat's *currently checked-out* branch is already represented on
the rig's default branch:

1. Fast path: `verifyCommitOnMain` — plain ancestor check against every
   remote's default branch (catches fast-forward merges).
2. `git.BranchTargetStatus(branch, remote, []string{remote+"/"+defaultBranch})`
   → `preservationOfRefAgainstRef` (`internal/git/git.go:3683`), which tries,
   in order: ancestor, then a merge-tree no-op (squash merges), then
   `git cherry` patch-id equivalence (`CountCherryUnmergedCommits`). Only
   `UnpreservedPatchCount == 0` counts as preserved. Any git error at any
   step returns `false` (or a non-nil error), never a false "preserved".

This is exactly "by patch-id, not `--is-ancestor` alone" — it already exists,
is already tested, and is already used for a decision with real
consequences (skip-restart-and-archive in `handleZombieRestart`, gt-ho4f's
sibling bead's aa-apw fix). This design calls it as-is; it adds no new git
comparison logic to the primitive itself.

**It checks whatever is on disk, not the branch `gt done` recorded — this
design closes that gap before trusting the result (see New code §2).**
`_verifyBranchAlreadyMerged` calls `g.CurrentBranch()` against whatever is
checked out in the polecat's worktree at call time. For a `done` polecat
that has not been touched since completion this is the same branch `gt done`
recorded (`fields.Branch`), but nothing enforces that identity inside the
primitive itself, and the primitive's own stale-branch guard (gt-skwt,
below) is a no-op when called with `hookBead=""`. Calling it with
`hookBead=""` blindly, the way an earlier draft of this design did, means a
worktree that happens to be checked out to the default branch, detached, or
some unrelated branch would verify as "preserved" without saying anything
about the specific work this polecat completed. New code §2 adds a
branch-identity check ahead of the call, plus passes a non-empty `hookBead`,
to close this instead of relying on the primitive alone.

It also already guards against a stale on-disk branch belonging to a
superseded assignment (gt-skwt): if a non-empty `hookBead` is passed and the
checked-out branch's embedded issue doesn't match, it returns `false, nil`
without checking anything else. This design's promotion path passes
`fields.LastSourceIssue` (the completed work's issue ID, preserved after
`hook_bead` is cleared — see `internal/beads/beads_agent.go:58`) as
`hookBead`, so this guard is active for promotion, not inert.

**The single-writer CAS pattern already exists.**
`internal/beads/beads_agent.go:576` (`ClearAgentActiveMRIfMatches`) is the
precedent: lock the agent bead (`lockAgentBead`, an flock per bead ID),
`Show`, `ParseAgentFields`, re-check the precondition against the freshly
read fields, write only if it still holds, all inside one lock hold. Every
agent-bead field writer funnels through this same lock
(`UpdateAgentDescriptionFields`, `internal/beads/beads_agent.go:483`), so a
promotion write and a concurrent reuse-sling's `working`+`hook_bead` write
cannot interleave — whichever acquires the lock first is authoritative, and
the loser's read (which happens after the winner's write commits) sees the
post-write state and re-evaluates correctly. `internal/beads/agent_bead_guard_test.go`
statically enforces that agent_state writes go through `.ForAgentBead()`
first — the new method must go through the same lock and target-resolution
path `ClearAgentActiveMRIfMatches` uses (`b.agentBeadTarget()` redirect,
`b.lockAgentBead(id)`), not a bare `UpdateAgentState` call, and must be added
to the `agentBeadHelpers` list that test enforces against (that list does
not yet include `ClearAgentActiveMRIfMatches`'s sibling; both should be
present so a bare `beads.New()` call chain is statically caught for either).

## New code

### 1. `internal/beads/beads_agent.go` — `PromoteAgentDoneToIdle`

Added next to `ClearAgentActiveMRIfMatches`, same shape:

```go
// PromoteAgentDoneToIdle moves an agent bead from done to idle, but only if
// every bookkeeping gate still holds under the same lock+read that performs
// the write (closing the TOCTOU window against a concurrent reuse-sling).
// Callers are responsible for the external, non-bead evidence (patch-id
// verification that the polecat's branch is already on the default branch)
// BEFORE calling this — that evidence cannot become stale between the
// caller's check and this call (a merge cannot un-land), so it is not
// re-checked here. What CAN change between the caller's check and this call
// is the bead itself, and that is exactly what this re-checks.
//
// Returns true only if the write happened. A false return with a nil error
// means some gate no longer held — the caller should treat this exactly
// like "not yet verified" (stays done), not as a failure.
func (b *Beads) PromoteAgentDoneToIdle(id string) (bool, error) {
	if target := b.agentBeadTarget(); target != b {
		return target.PromoteAgentDoneToIdle(id)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}

	fl, lockErr := b.lockAgentBead(id)
	if lockErr != nil {
		return false, fmt.Errorf("locking agent bead %s: %w", id, lockErr)
	}
	defer func() { _ = fl.Unlock() }()

	issue, err := b.Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if !IsAgentBead(issue) {
		return false, fmt.Errorf("%s is not an agent bead", id)
	}

	fields := ParseAgentFields(issue.Description)
	if fields.AgentState != string(AgentStateDone) {
		return false, nil
	}
	if strings.TrimSpace(fields.HookBead) != "" {
		return false, nil
	}
	if strings.TrimSpace(fields.ActiveMR) != "" {
		return false, nil
	}
	if fields.CleanupStatus != "clean" {
		return false, nil
	}

	fields.AgentState = string(AgentStateIdle)
	// Self-heal: a successful patch-id-verified landing is proof the branch
	// reached the default branch, which is strictly stronger evidence than
	// whatever PushFailed/MRFailed recorded about an earlier attempt. Leaving
	// them true here would incorrectly keep flagging a polecat whose work
	// we just proved is safely landed. See "Consumers that treat idle
	// differently from done" (#1) for how this interacts with
	// brokenIdleReclaimAgentBlocker.
	fields.PushFailed = false
	fields.MRFailed = false

	description := FormatAgentDescription(issue.Title, fields)
	if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
		return false, err
	}
	telemetry.RecordAgentStateChange(context.Background(), id, string(AgentStateIdle), nil, nil)
	return true, nil
}
```

`CleanupStatus == "clean"` is an exact-string gate, not routed through the
gt-ui2x/`ResolveIgnoreCleanupStatus` relaxation machinery. Missing/unknown
fails closed here, deliberately, per gt-14a/gt-7kr's lesson and independent
of whatever that subsystem does for reuse eligibility — this gate answers a
different question (is it safe to call this "idle" for reporting purposes)
and must not inherit that subsystem's relaxations or its bugs.

The telemetry call is unconditional here (only reached after a successful
write) rather than deferred the way `UpdateAgentState` defers it — this
function has multiple no-op early returns (`false, nil`) that are not state
transitions and must not be recorded as one.

### 2. `internal/witness/idle_promotion.go` (new file) — `PromoteVerifiedPolecats`

Orchestration, modeled on `DetectZombiePolecats`'s and `DiscoverCompletions`'s
existing scan shape (`os.ReadDir(polecatsDir)`, per-polecat agent bead ID via
`beads.PolecatBeadIDWithPrefix`):

```go
type PromotionResult struct {
	PolecatName string
	AgentBeadID string
	Promoted    bool
	Reason      string // "" on promotion; else why it was skipped/errored
	Error       error
}

// PromoteVerifiedPolecatsResult.Results holds one entry per polecat that
// reached the git-verification stage (i.e. passed the cheap bookkeeping
// gate) — promoted, skipped, and errored outcomes alike. Filter on
// PromotionResult.Promoted for the subset actually written. Polecats that
// never reached verification (failed the cheap gate, had a live session)
// are counted in Skipped but do not get a Results entry — there is nothing
// per-polecat to report beyond "not eligible yet".
type PromoteVerifiedPolecatsResult struct {
	Checked int
	Results []PromotionResult
	Skipped int
	Errors  []error
}

func PromoteVerifiedPolecats(bd *BdCli, workDir, rigName string) *PromoteVerifiedPolecatsResult {
	result := &PromoteVerifiedPolecatsResult{}
	// ... townRoot/initRegistryFromTownRoot/polecatsDir read, identical
	// pattern to DetectZombiePolecats (handlers.go:1676-1702) ...

	t := tmux.NewTmux()
	for _, polecatName := range polecatNames {
		agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
		result.Checked++

		snap := fetchAgentBeadSnapshot(workDir, agentBeadID)
		if snap == nil || beads.AgentState(snap.AgentState) != AgentStateDone {
			continue
		}
		// snap.Fields.HookBead (description-parsed), not snap.HookBead
		// (issue.HookBead, the bead's own hook column) — hq-l6mm5: that
		// column is no longer maintained. PromoteAgentDoneToIdle's own gate
		// reads fields.HookBead too, so the cheap pre-check and the locked
		// check must agree on which field they mean.
		fields := snap.Fields
		if fields == nil || strings.TrimSpace(fields.HookBead) != "" ||
			strings.TrimSpace(fields.ActiveMR) != "" || fields.CleanupStatus != "clean" {
			result.Skipped++
			continue
		}

		// Defense in depth: a live session on a "done" polecat is itself an
		// anomaly the zombie/submitted-still-running detectors own — don't
		// race them by promoting underneath a session that might be mid-reuse.
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
		if alive, err := t.HasSession(sessionName); err != nil || alive {
			result.Skipped++
			continue
		}

		// Verify the branch actually on disk is the branch gt done recorded
		// before trusting a patch-id check against it — see "checks
		// whatever is on disk, not the branch gt done recorded" above.
		onDisk, err := checkedOutBranch(workDir, rigName, polecatName)
		if err != nil || strings.TrimSpace(fields.Branch) == "" || onDisk != fields.Branch {
			result.Skipped++
			continue // fail closed — can't attribute the on-disk state to this completion
		}

		merged, err := verifyBranchAlreadyMerged(workDir, rigName, polecatName, fields.LastSourceIssue)
		pr := PromotionResult{PolecatName: polecatName, AgentBeadID: agentBeadID}
		switch {
		case err != nil:
			pr.Reason = fmt.Sprintf("verify-error: %v", err)
			result.Skipped++
		case !merged:
			pr.Reason = "not-landed"
			result.Skipped++
		default:
			promoted, promoteErr := beads.New(workDir).ForAgentBead().PromoteAgentDoneToIdle(agentBeadID)
			switch {
			case promoteErr != nil:
				pr.Error = promoteErr
				result.Errors = append(result.Errors, fmt.Errorf("promoting %s: %w", polecatName, promoteErr))
			case promoted:
				pr.Promoted = true
			default:
				pr.Reason = "gate no longer held at write time (race)"
				result.Skipped++
			}
		}
		result.Results = append(result.Results, pr)
	}
	return result
}

// checkedOutBranch resolves a polecat's worktree the same way
// _verifyBranchAlreadyMerged does and returns its currently checked-out
// branch. Kept separate from that primitive (rather than changing its
// signature or return value) so this design touches zero lines of an
// already-hardened, already-tested function.
func checkedOutBranch(workDir, rigName, polecatName string) (string, error) {
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return "", fmt.Errorf("finding town root: %v", err)
	}
	polecatPath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if _, err := os.Stat(polecatPath); os.IsNotExist(err) {
		polecatPath = filepath.Join(townRoot, rigName, "polecats", polecatName)
	}
	return git.NewGit(polecatPath).CurrentBranch()
}
```

Before calling `verifyBranchAlreadyMerged`, best-effort fetch the default
branch with a bounded timeout (`git.FetchDefaultBranchWithTimeout`, already
exists, ignore its error) — promotion is not on any hot/latency-sensitive
path the way the zombie-restart check is, so it can afford to reduce false
negatives from a stale local remote-tracking ref instead of waiting for
some other code path to fetch first. A failed fetch does not block
verification; it just means verification runs against whatever refs are
already local, same as today's zombie-restart callers.

### 3. `internal/cmd/patrol_scan.go` — wire it into `gt patrol scan`

Add as a sixth phase after completion discovery
(`runPatrolScanPhase(diagnostics, "completion discovery", ...)`,
`internal/cmd/patrol_scan.go:253`) and before the real-activity observation
phase (which is deliberately last "so it reflects state after the automatic
actions above" — promotion is an automatic action and belongs before it,
same as the others):

```go
promotionResult := runPatrolScanPhase(diagnostics, "idle promotion", func() *witness.PromoteVerifiedPolecatsResult {
	return witness.PromoteVerifiedPolecats(bd, workDir, rigName)
})
```

Threaded through `outputPatrolScanJSON`/`outputPatrolScanHuman` as a fifth
result argument, same as the existing four.

### 4. Formula/doc text (prose only, no behavior)

`internal/formula/formulas/mol-witness-patrol.formula.toml`'s lifecycle
section currently says (per gt-iljx's own citation) that no code path
promotes done->idle. Update it to describe the new phase in one line:
"idle promotion: a done polecat with no hook, no active MR, and a clean
self-report is promoted to idle once its recorded branch is verified (by
patch-id) already on the default branch." Same update to any other doc that
repeats the "nothing promotes done->idle" claim gt-ho4f's fix introduced
(grep `done->idle\|done to idle` across `internal/witness/handlers.go`
comments and `internal/polecat/types.go`).

## Proposed state-machine doc text (for `internal/polecat/types.go`)

Replace the `StateDone` doc comment (currently: "No code path promotes done
to idle today (gt-iljx)...") with:

```go
// StateDone means the polecat has completed its assigned work and called
// 'gt done'; its session has exited. Done is fully reuse-eligible on its own
// (see IsReuseEligible) — nothing about being "done" rather than "idle"
// blocks reslinging. The witness promotes a done polecat to idle once it has
// verified, at source, that the recorded completion branch is durably landed
// (gt-glfh): no hook_bead, no active_mr, cleanup_status=clean, and the
// polecat's recorded branch (fields.Branch, matched against what is
// currently checked out) is patch-id-preserved on the rig's default branch
// (internal/witness/idle_promotion.go, PromoteVerifiedPolecats). Any gate
// unmet or unmeasurable leaves the polecat in done — done is the fail-closed
// state, idle is the verified one. A polecat can sit in done indefinitely
// (e.g. an open MR still in queue) without that being a problem. Promotion
// also makes the polecat reachable by idle-only consumers it was not
// reachable by as done (ReclaimBrokenIdlePolecat, cleanup_status reconcile
// — see the design doc's "Consumers that treat idle differently from done"
// for why each is safe).
StateDone State = "done"
```

And the `StateIdle` comment gains one clause: "...no pending completion
cleanup state. Reached either by never having worked yet, or by witness
promotion from done once verified (see StateDone)."

## Tests

All in `internal/beads` and `internal/witness`, no new integration harness:

1. `TestPromoteAgentDoneToIdle_PromotesWhenAllGatesHold` — done, no hook, no
   active_mr, cleanup_status=clean → returns `true, nil`; re-`Show` confirms
   `agent_state=idle`.
2. `TestPromoteAgentDoneToIdle_NotDoneStaysAsIs` — `agent_state=working` →
   `false, nil`, state unchanged. This is the direct regression test for
   "promotion happens only after verification, not before" at the write
   layer: a caller that (incorrectly) called this before the polecat even
   finished must be a no-op.
3. `TestPromoteAgentDoneToIdle_PendingMRStaysDone` — done, `active_mr` set →
   `false, nil`, state stays `done`. This is the bead's named acceptance
   case ("a polecat with pending MR stays done").
4. `TestPromoteAgentDoneToIdle_HookBeadSetStaysDone` and
   `TestPromoteAgentDoneToIdle_DirtyCleanupStatusStaysDone` (table test over
   `has_uncommitted`/`has_stash`/`has_unpushed`/empty/`CleanupUnknown`) —
   each gate pinned individually so a future change can't silently drop one.
5. `TestPromoteAgentDoneToIdle_ClearsStaleFailureFlagsOnPromotion` — done
   with `PushFailed=true` or `MRFailed=true` plus all other gates clean →
   promotes, and re-`Show` confirms both flags are false afterward. Pins the
   self-heal behavior documented in New code §1 and in "Consumers that treat
   idle differently from done" (#1).
6. `TestPromoteAgentDoneToIdle_RaceConcurrentReuseWins` — write the bead to
   `working`+hook set between an external caller's snapshot read and the
   `PromoteAgentDoneToIdle` call (simulating a sling that won the race) →
   `false, nil`, state stays `working` (never clobbered back to idle).
7. `TestPromoteVerifiedPolecats_PromotesOnlyAfterGitVerification` — inject a
   fake `verifyBranchAlreadyMerged` (already a package var, test-overridable
   exactly like the zombie-restart tests do today) returning
   `(true, nil)` for one fixture polecat and `(false, nil)` for another with
   identical bookkeeping state and matching on-disk/recorded branches → only
   the first is promoted; the second's `Results` entry has
   `Reason == "not-landed"`.
8. `TestPromoteVerifiedPolecats_GitVerificationErrorFailsClosed` — injected
   verify func returns a non-nil error → not promoted, no panic, no entry in
   `result.Errors` (a verification miss is routine, not exceptional — only
   the CAS write's own error goes to `result.Errors`), and the polecat's
   `Results` entry has `Reason` starting with `"verify-error: "` so the miss
   is attributable instead of silently indistinguishable from "not merged
   yet".
9. `TestPromoteVerifiedPolecats_SkipsBeforeGitCheckWhenBookkeepingGateFails`
   — assert the injected verify func is never called when
   `fields.HookBead`/`fields.ActiveMR`/`fields.CleanupStatus` already fail
   the cheap gate (spy/counter on the fake), so a polecat that obviously
   isn't eligible never pays for a git probe.
10. `TestPromoteVerifiedPolecats_SkipsLiveSession` — tmux session alive for a
    "done" polecat → skipped, verify func not called.
11. `TestPromoteVerifiedPolecats_SkipsWhenOnDiskBranchDiffersFromRecorded` —
    fixture polecat's agent bead records `fields.Branch = "polecat/x/gt-aaaa+1"`
    but the fake `checkedOutBranch` returns a different branch (simulating a
    reused/reassigned worktree whose checkout moved on after completion) →
    skipped before `verifyBranchAlreadyMerged` is ever called, `Results`
    entry Reason is non-empty. This is the direct regression test for
    "verification checks whichever branch happens to be checked out, not the
    branch recorded at completion".
12. `TestPromoteVerifiedPolecats_PassesLastSourceIssueAsHookBead` — assert
    `verifyBranchAlreadyMerged` is invoked with `hookBead ==
    fields.LastSourceIssue`, not `""`, so the gt-skwt stale-assignment guard
    inside the primitive is live for promotion calls.
13. Explicit non-regression: `internal/polecat/manager_test.go`'s
    `TestFindIdlePolecat_AcceptsDoneCandidateWithZeroIdle` (gt-uu6) is not
    touched by this design and must still pass unmodified — this design adds
    no code in `internal/polecat/manager.go` or `workstate.go`.
14. `internal/beads/agent_bead_guard_test.go`'s static guard test: add
    `PromoteAgentDoneToIdle` (and, while touching this list, its existing
    sibling `ClearAgentActiveMRIfMatches`, which the guard test does not yet
    cover either) to `agentBeadHelpers` so a bare `beads.New()...` call
    chain bypassing the lock is statically caught for both.

## Failure modes considered and rejected

- **Writing idle from the refinery's post-merge path
  (`internal/refinery/terminal_mr.go:closeTerminalMR`) instead of a witness
  patrol scan.** Rejected: mayor's directive is explicit that the witness is
  the verifier, and a second writer of `agent_state` from the refinery would
  violate "one writer" and reintroduce exactly the dual-writer race this
  design's CAS is built to avoid. The refinery's existing
  `ClearAgentActiveMRIfMatches` call already clears `active_mr` at that
  moment, which is sufficient signal for the witness's next patrol pass —
  promotion does not need to be synchronous with the merge.
- **Re-verifying git state inside the bead lock.** Rejected: the lock is
  per-agent-bead and held by every writer town-wide serially through
  `lockAgentBead`; holding it across a git subprocess (and potentially a
  network `ls-remote`) would serialize unrelated agent-bead writes behind
  git I/O latency. Not needed anyway — a confirmed patch-id landing cannot
  become false later, so the git check is safe to do before acquiring the
  lock and not repeat inside it.
- **Routing cleanup_status through `ResolveIgnoreCleanupStatus` /
  `RecordedCleanupBlocks` (the gt-ui2x machinery) instead of a literal
  string check.** Rejected: that machinery answers "is it safe to reuse/
  nuke this seat", a related but different question, and it has already
  needed three corrective review passes this cycle. Reusing it here would
  couple this design's correctness to that subsystem's next change. A
  literal `== "clean"` check is small enough to review on its own and fails
  closed on every value that isn't an explicit "clean" self-report.
- **Treating `PushFailed`/`MRFailed` as permanent gates instead of
  self-healing them on promotion.** Rejected: neither field has any other
  writer that clears it (confirmed by reading `clearCompletionMetadata`,
  `internal/witness/handlers.go:3018` — it clears `ExitType`/`MRID`/
  `CompletionTime` and conditionally `Branch`, but never `PushFailed`/
  `MRFailed`), so gating on them without clearing them would make a
  polecat that had one recorded push failure stuck in `done` forever even
  after the work demonstrably landed. A successful patch-id verification is
  strictly stronger evidence than a stale failure flag from an earlier
  attempt, so promotion clears both. This also removes two of
  `brokenIdleReclaimAgentBlocker`'s checks at the moment of promotion — see
  "Consumers that treat idle differently from done" (#1) for why that
  specific interaction is safe rather than an oversight.
- **Verifying only against the currently checked-out branch, with
  `hookBead=""`, the way the first draft of this design did.** Rejected
  after review: this makes the verification result meaningless whenever the
  on-disk branch is not the branch `gt done` recorded (default branch
  checked out, detached HEAD, or a superseded reassignment). New code §2
  instead compares the on-disk branch against `fields.Branch` before
  trusting the result, and passes `fields.LastSourceIssue` as `hookBead` so
  the primitive's own gt-skwt guard is active.

## Non-goals

- Does not change `FindIdlePolecat`, `IsReuseEligible`, or `DecideWorkstate`
  (gt-uu6's `done`-as-reusable behavior is untouched and unregressed).
- Does not change the reuse/nuke dirt-check or cleanup_status-missing
  recovery path gt-ui2x just landed.
- Does not make promotion synchronous with merge; it is a patrol-cycle-
  latency (next `gt patrol scan`) background reconciliation, same latency
  class as zombie detection and state-collapse detection.
- Does not resurrect the deleted `TransitionPolecatToIdle` function by that
  name — `PromoteAgentDoneToIdle` is a new, narrower function with a CAS
  precondition the old one never had (the old one was an unconditional
  write with zero callers).
- Does not change `_verifyBranchAlreadyMerged`/`verifyBranchAlreadyMerged`,
  `BranchTargetStatus`, or `preservationOfRefAgainstRef` — the branch-
  identity check this design adds (`checkedOutBranch`) lives entirely in the
  new `idle_promotion.go` file.

## Revision history

- 2026-09-22: initial draft (MR gt-wisp-v8j9). Rejected by editorial review,
  score 0.52, 9 findings (4 major) — see gt-glfh notes.
- 2026-09-23 (this revision): addresses all 9 attempt-1 findings —
  corrected the "not on any destructive path" claim and added "Consumers
  that treat idle differently from done"; fixed branch verification to
  check the recorded completion branch and pass `LastSourceIssue` as
  `hookBead`; added the `> Status:` header (docs-lint R12); aligned the
  cheap pre-check to read `hook_bead` from the same place
  (`fields.HookBead`) as the locked check; added
  `PromoteAgentDoneToIdle`/`ClearAgentActiveMRIfMatches` to the static guard
  test's helper list; added the telemetry call on promotion; gave
  verification-error skips a distinguishable `Reason`; corrected the
  `clearCompletionMetadata`/`preservationOfRefAgainstRef` line references
  and de-duplicated the grep pattern; renamed
  `PromoteVerifiedPolecatsResult.Promoted` to `Results` since it always held
  skip/error entries alongside real promotions.
