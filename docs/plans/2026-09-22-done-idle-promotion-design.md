# done -> idle promotion after witness verification

Date: 2026-09-22. Bead: gt-glfh. Design only — no implementation in this pass
(mayor's instruction on gt-glfh: "Do not implement without a reviewed spec").

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
skip), and it is not on any destructive path.

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
whether a polecat's current branch is already represented on the rig's
default branch:

1. Fast path: `verifyCommitOnMain` — plain ancestor check against every
   remote's default branch (catches fast-forward merges).
2. `git.BranchTargetStatus(branch, remote, []string{remote+"/"+defaultBranch})`
   → `preservationOfRefAgainstRef` (`internal/git/git.go:3686`), which tries,
   in order: ancestor, then a merge-tree no-op (squash merges), then
   `git cherry` patch-id equivalence (`CountCherryUnmergedCommits`). Only
   `UnpreservedPatchCount == 0` counts as preserved. Any git error at any
   step returns `false` (or a non-nil error), never a false "preserved".

This is exactly "by patch-id, not `--is-ancestor` alone" — it already exists,
is already tested, and is already used for a decision with real
consequences (skip-restart-and-archive in `handleZombieRestart`, gt-ho4f's
sibling bead's aa-apw fix). This design calls it as-is; it adds no new git
comparison logic.

It also already guards against a stale on-disk branch belonging to a
superseded assignment (gt-skwt): if a non-empty `hookBead` is passed and the
checked-out branch's embedded issue doesn't match, it returns `false, nil`
without checking anything else. The promotion path always calls it with
`hookBead=""` (promotion is gated on `HookBead==""` already), so that guard
is inert here — acceptable, because the question promotion asks is "is
whatever is currently checked out here already safely landed", not
"was it landed for a specific issue."

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
`b.lockAgentBead(id)`), not a bare `UpdateAgentState` call.

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
	// we just proved is safely landed.
	fields.PushFailed = false
	fields.MRFailed = false

	description := FormatAgentDescription(issue.Title, fields)
	if err := b.Update(id, UpdateOptions{Description: &description}); err != nil {
		return false, err
	}
	return true, nil
}
```

`CleanupStatus == "clean"` is an exact-string gate, not routed through the
gt-ui2x/`ResolveIgnoreCleanupStatus` relaxation machinery. Missing/unknown
fails closed here, deliberately, per gt-14a/gt-7kr's lesson and independent
of whatever that subsystem does for reuse eligibility — this gate answers a
different question (is it safe to call this "idle" for reporting purposes)
and must not inherit that subsystem's relaxations or its bugs.

### 2. `internal/witness/idle_promotion.go` (new file) — `PromoteVerifiedPolecats`

Orchestration, modeled on `DetectZombiePolecats`'s and `DiscoverCompletions`'s
existing scan shape (`os.ReadDir(polecatsDir)`, per-polecat agent bead ID via
`beads.PolecatBeadIDWithPrefix`):

```go
type PromotionResult struct {
	PolecatName string
	AgentBeadID string
	Promoted    bool
	Reason      string // "" on promotion; else why it was skipped
	Error       error
}

type PromoteVerifiedPolecatsResult struct {
	Checked   int
	Promoted  []PromotionResult
	Skipped   int
	Errors    []error
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
		if snap.HookBead != "" || snap.ActiveMR != "" || snap.cleanupStatus() != "clean" {
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

		merged, err := verifyBranchAlreadyMerged(workDir, rigName, polecatName, "")
		if err != nil || !merged {
			result.Skipped++
			continue // fail closed — unverifiable or not-yet-landed stays done
		}

		promoted, err := beads.New(workDir).ForAgentBead().PromoteAgentDoneToIdle(agentBeadID)
		pr := PromotionResult{PolecatName: polecatName, AgentBeadID: agentBeadID}
		if err != nil {
			pr.Error = err
			result.Errors = append(result.Errors, fmt.Errorf("promoting %s: %w", polecatName, err))
		} else if promoted {
			pr.Promoted = true
		} else {
			pr.Reason = "gate no longer held at write time (race)"
			result.Skipped++
		}
		result.Promoted = append(result.Promoted, pr)
	}
	return result
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
self-report is promoted to idle once its branch is verified (by patch-id)
already on the default branch." Same update to any other doc that repeats
the "nothing promotes done->idle" claim gt-ho4f's fix introduced (grep
`done->idle\|done->idle` across `internal/witness/handlers.go` comments
and `internal/polecat/types.go`).

## Proposed state-machine doc text (for `internal/polecat/types.go`)

Replace the `StateDone` doc comment (currently: "No code path promotes done
to idle today (gt-iljx)...") with:

```go
// StateDone means the polecat has completed its assigned work and called
// 'gt done'; its session has exited. Done is fully reuse-eligible on its own
// (see IsReuseEligible) — nothing about being "done" rather than "idle"
// blocks reslinging. The witness promotes a done polecat to idle once it has
// verified, at source, that the work is durably landed (gt-glfh):
// no hook_bead, no active_mr, cleanup_status=clean, and the polecat's
// current branch is patch-id-preserved on the rig's default branch
// (internal/witness/idle_promotion.go, PromoteVerifiedPolecats). Any gate
// unmet or unmeasurable leaves the polecat in done — done is the fail-closed
// state, idle is the verified one. A polecat can sit in done indefinitely
// (e.g. an open MR still in queue) without that being a problem.
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
5. `TestPromoteAgentDoneToIdle_RaceConcurrentReuseWins` — write the bead to
   `working`+hook set between an external caller's snapshot read and the
   `PromoteAgentDoneToIdle` call (simulating a sling that won the race) →
   `false, nil`, state stays `working` (never clobbered back to idle).
6. `TestPromoteVerifiedPolecats_PromotesOnlyAfterGitVerification` — inject a
   fake `verifyBranchAlreadyMerged` (already a package var, test-overridable
   exactly like the zombie-restart tests do today) returning
   `(true, nil)` for one fixture polecat and `(false, nil)` for another with
   identical bookkeeping state → only the first is promoted.
7. `TestPromoteVerifiedPolecats_GitVerificationErrorFailsClosed` — injected
   verify func returns a non-nil error → not promoted, no panic, error
   surfaced in `result.Errors` is NOT required (a verification miss is
   routine, not exceptional — only the CAS write's own error goes to
   `result.Errors`).
8. `TestPromoteVerifiedPolecats_SkipsBeforeGitCheckWhenBookkeepingGateFails`
   — assert the injected verify func is never called when hook_bead/
   active_mr/cleanup_status already fail the cheap gate (spy/counter on the
   fake), so a polecat that obviously isn't eligible never pays for a git
   probe.
9. `TestPromoteVerifiedPolecats_SkipsLiveSession` — tmux session alive for a
   "done" polecat → skipped, verify func not called.
10. Explicit non-regression: `internal/polecat/manager_test.go`'s
    `TestFindIdlePolecat_AcceptsDoneCandidateWithZeroIdle` (gt-uu6) is not
    touched by this design and must still pass unmodified — this design adds
    no code in `internal/polecat/manager.go` or `workstate.go`.

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
  `internal/witness/handlers.go:3016` — it clears `ExitType`/`MRID`/
  `CompletionTime` and conditionally `Branch`, but never `PushFailed`/
  `MRFailed`), so gating on them without clearing them would make a
  polecat that had one recorded push failure stuck in `done` forever even
  after the work demonstrably landed. A successful patch-id verification is
  strictly stronger evidence than a stale failure flag from an earlier
  attempt, so promotion clears both.

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
