# Overnight-watch fixes: throughput, routing, mayor ground truth, config-moves-with-code

Date: 2026-09-19. Source: handoff bead hq-5n25a (findings from the operator watch 2026-09-18 20:50 to 2026-09-19 09:50). Decisions were grilled and approved by Sloan on 2026-09-19; the grilling record is in the bead notes.

## Destination

Gas Town lands merged work at three or more MRs per hour with the ready queue held under about eight, keeps all three local model slots busy with work the local model handles well, gives the mayor one command that reports process truth, and never again needs a human to re-stamp a manifest, un-park a plugin, or discover a flattened database by accident. The route is done when a full night runs on these rules and the sampler shows the numbers.

## Baseline (the night of 2026-09-18)

| Measure | Value |
|---|---|
| Merges | 19 in 13.5 h (1.4/h) |
| Queue peak / drained to | 19 / 6, only because slings were paused |
| Merges that were P0 or P1 | 16 of 19 |
| Local slot-seconds used, Coder-Next era | 29% of three slots |
| Samples with zero busy local slots | 36% |
| Samples with all three busy | 2% |
| Beads that cycled polecat+gate 2-3 times | 4 |
| Manual manifest re-stamps | 3 |
| Unplanned Dolt flattens | 2 databases (third night running) |

Sampler: `~/.claude/tools/town-stats.sh`, output `~/.claude/docs/research/throughput-20260918/stats.tsv`, still running.

## What the evidence changed

Three of the original findings pointed at the wrong mechanism. The design below targets the verified ones.

- Batch gating was already built, enabled, and idle. Two causes: the batch path excludes P0 and P1 by design (`internal/cmd/mq_batch.go:152`) while the town files most work at P1, and the refinery read stale formula text saying batching fails on editorial-required rigs "until T7 lands" and chose the single-MR path. T7 landed on 09-11. The refinery never ran `gt mq batch candidates` overnight.
- The 03:03 flatten was the daemon's `scheduled_maintenance` patrol running `gt maintain --force`, not the compactor dog. `gt maintain` has no remote-divergence check and does not push. The gt database has a Dolt remote, so local and remote history now disagree.
- The witness's short-session burst came from its role template ("hand off immediately after any extraordinary action"), not from context pressure. Its formula only hands off at context HIGH. Separately, the town-tier witness formula file is stale at the same version number as the repo's and is missing the stall-judgment rules that landed after the opal false restart.

## Workstreams

E (dashboard) was added after Sloan's review on 2026-09-19: the web dashboard at 127.0.0.1:8080 is the human channel for the same truth the mayor lacks, and its standing operator bead hq-6sbi7 tracks verification. Its scope is in the plan (Task 10): a merge-queue panel fed by merge-request wisps instead of GitHub pull requests, a local-pool panel from the llama-server slots endpoint, a gate panel from `gt slot status`, and the polecat panel's agent and MR columns from workstream C1.

Each workstream states the decision, the design, the seams (verified file:line on main 4720428), the failure modes it must not introduce, how it is tested, and who lands it. "Operator" means this session by hand after approval; "convoy" means rig beads slung by the mayor.

### A. Throughput

**A1. Batch P1 and P2; enforce the batch threshold in code.** Change the exclusion at `mq_batch.go:152` to `Priority == 0`. Move `batch_min_count` from formula prose into `runMQBatchRun` (`mq_batch.go:275`): if eligible candidates are fewer than the configured minimum, print the count and exit without assembling. Set `batch_min_age` to 20m in `gastown/config.json`. Rewrite the formula's batch-scan Step 2 (`mol-refinery-patrol.formula.toml:393-415`) to describe what `gt mq batch run` does today: it reviews each member, drops a member that gets request_changes or an infra failure and leaves it queued, ejects a member whose patch-id changed on the stack, runs one suite on the tip, rebuilds once on red, then bisects. Keep Step 4 (MERGED mail per landed MR) and Step 6 (continue to the single-MR path so P0 never starves). Failure mode to guard: a red batch bisects with a full suite per probe, so cap `batch_max` at 6 until the de-flake work lands. Test: unit test on the filter for P1 inclusion and P0 exclusion; a min-count table test; the formula change is verified by the refinery's next batch-scan output showing a candidates call. Owner: convoy for the Go change; operator for the formula text (repo file plus the town-tier copy) and config.

**A2. De-flake epic.** One P1 epic with one child per package: internal/cmd (gt-v2a5, gt-5clb, gt-fhkg-adjacent), internal/refinery (gt-kyct, gt-bzkt), internal/config (gt-5v82), internal/polecat (gt-nyh8, gt-e1u5), internal/nudge (gt-v25y, gt-38ss, gt-k9sb), internal/tmux (gt-rdvq, gt-xt7o, gt-n0jv), internal/mayor (gt-toc1), plus gt-taoz (lint lock serialisation) and gt-ul8f. Each child's acceptance: the named tests pass ten times under `-count=10` with the full suite running in parallel on the same host, and the fix asserts bounds rather than observed timing. Flash polecats. Owner: convoy.

**A3. Sling backpressure.** Directive today in `gastown/directives/refinery.md` sibling `dispatch.md`: no new slings while the ready queue exceeds 12, except rework and rubric MRs. Code: a `merge_queue.max_ready_for_dispatch` knob (default 0 = off) read in `SpawnPolecatForSling` before the pool decision (`polecat_spawn.go:138`); when the rig's ready count exceeds it, `gt sling` refuses with the count unless the bead carries a `rework` label or `--force` is passed. The convoy feeder (`internal/daemon/convoy*.go`) reads the same knob and defers feeding instead of failing. Failure mode: the feeder must not mark a deferred bead as failed or strand it; test that a deferred bead is re-offered on the next tick. Owner: operator for the directive; convoy for the code.

**A4. Main-branch test skips when the gate is busy.** Before setup (fetch and worktree add currently run before the slot at `main_branch_test_runner.go:311-324`), call `slot.StatusPool` and skip the rig with a logged reason when any `<rig>/refinery` or `<rig>/refinery-batch` role holds a slot; retry on the next tick. A cycle where every rig skipped must log as skipped, not as "1 tested, 0 failed". Owner: convoy (gt-lf2r).

**A6. Sling dispatch latency (found by the mayor on 2026-09-19).** A real sling takes minutes, not seconds: the convoy feeder fed gt-ipk7 at 10:20:19 and the polecat's tmux session was created at 10:31:34, eleven minutes later. A dry-run sling completes in 1.3 s and `bd show` answers in 0.26 s, so neither the pool decision nor the bead store is the cost; the time is inside the real spawn path between `SpawnPolecatForSling` and `StartSession` (worktree creation, admission, convoy creation, formula cook and bond, hook writes with retry). No step timing exists today. Design: add a `[sling] step <name> took <d>` log line after each step in `polecat_spawn.go` and the convoy feeder, then fix whichever step dominates. Until measured this is a research ticket on the map; the mayor's slings are the instrument. Owner: convoy for the timing lines; research ticket for the diagnosis.

**A5. Two refineries on one container-gate pool.** The pool is 4 slots with 1 reserved for gate-class roles; om's refinery is now live and shares that one reserved slot with gastown's refinery and main-branch-test. Docker VM is 24 vCPU and 8 GiB. This is a research ticket on the wayfinder map: measure peak memory of one gastown suite and one om suite, then set `reserved_for_gate` to the number of active refineries or make the reservation per rig. No change until measured.

### B. Routing and local-slot utilisation

**B1. Route by bead shape.** There is no bead-attribute routing today; `choosePoolAgent` (`sling_pool.go:31`) takes only the pool and the live sessions. Widen `resolvePolecatPoolAgent` to take the hooked bead (already in scope as `opts.HookBead` at `polecat_spawn.go:138`) and apply, in order: a `route:local` or `route:flash` label wins; otherwise type `bug` or `feature` goes to the overflow agent and type `task`, `chore`, `docs`, or any bead whose MR carries prior findings (rework) goes local; then the existing seat count and stagger apply. Print the decision on one line that always names the chosen agent and the reason, which closes gt-ipk7: `pool: local seat 2/3 -> local-coder-polecat (type=task)` and `pool: overflow -> deepseek-flash (type=bug)` and `pool: local full (3/3) -> deepseek-flash`. Test: table test over label, type, seat count, and gap. Owner: convoy.

**B2. Pending-spawn reservation (gt-eoi9, already hooked).** The reservation must carry the chosen agent, because `AgentFields` has no agent field and the seat count filters on agent. Merge reservations into `listPolecatSessions` (`sling_pool.go:75-97`) as synthetic sessions with the agent and a creation time, expiring after five minutes on the pattern at `daemon.go:2878-2893`. The polecat working gt-eoi9 gets this design as a comment. Owner: convoy (in flight).

**B3. Three local seats and a fill rule.** Set `polecat_pool.max_local` to 3. Sloan's preference is that local work displaces DeepSeek spend, so an idle local seat should not wait for a perfectly shaped bead: when a seat has been free for longer than `min_spawn_gap` and only overflow-shaped beads are queued, the next sling takes the seat locally with a `local-attempt:1` label; a bead whose first local attempt was rejected or stalled routes to flash on redispatch. Failure mode: a local logic bead looping through gates. The label plus the redispatch rule bounds it to one local attempt. Owner: operator for the config; convoy for the label rule inside B1.

**B4. Utilisation is measured, not assumed.** Add a `gpu` column to `town-stats.sh` from `ioreg -r -d 1 -c IOAccelerator` ("Device Utilization %", no sudo needed) and a `local_seats` column from the pool decision lines. Enable llama-server `--metrics` at the next planned restart (a restart costs every live local polecat a full re-prefill, so it rides along with the cache-ram trim to 12 GB). The operator's hourly report gains one line: busy-slot histogram and slot-seconds used for the last hour. The tuning question ("are we leaving power on the table") becomes a wayfinder ticket with the sampler as its instrument; the baseline answer is yes: 29% slot utilisation with two seats, the third seat unused. Owner: operator.

### C. Mayor ground truth

**C1. `gt polecat list` gains `agent` and `mr` columns.** Agent from the tmux session env `GT_AGENT` (read in `newPolecatSessionSet`, `polecat_inventory.go:37-47`, the same read `sling_pool.go` already pays for). MR state from one bulk `beads.ListMergeRequests` per rig after `bd.ListAgentBeads` (`polecat.go:517`), joined on `MRFields.Worker` or `AgentFields.ActiveMR`; never N calls to `bd show`, since MRs are ephemeral wisps and the per-id path is the blindness this workstream is removing. Both fields land in `PolecatListItem` and the JSON. Spawn grace for the stalled state (gt-yteq) applies at all three sites that synthesise it (`polecat_inventory.go:116`, `polecat.go:434`, `manager.go:2996`), using the agent bead's `spawning` state plus its update time and the existing `HeartbeatStartupGrace` (default 5m). Test: fixture with a hooked-but-not-started polecat reads `spawning`, not `stalled`; an idle polecat with a merged MR reads `idle-pr-merged`. Owner: convoy.

**C2. The other state-collapse detector is blind.** `DetectStateCollapse` (`state_collapse.go:70`) still uses plain `bd list --label=gt:merge-request`, which cannot see wisps, and prints an all-clear on zero results. Inject the same `ListOpenMRs` source `DetectStrandedBranches` now uses (`patrol_state_collapse.go:88-114`) and make a zero-MR result print "lookup returned 0 open MRs" rather than "no state collapse found". Re-scope gt-ke09 to this. Owner: convoy.

**C3. Memories reach the right roles at a bounded size.** In `gt prime`, render the memory index only for mayor and crew (role gate next to the mail gate at `prime.go:654-661`) and lower `memoryInjectMaxChars` to 3000 so the section fits the 9000-char hook budget instead of being the first thing evicted. Fix the namespace mismatch: `gt prime` indexes `gt.*` keys while `bd remember` writes `memory.*`, so either the index reads both prefixes or `gt remember` is the only front door; the index reads both. On the beads side set `prime.max-memory-chars` to 4000 in the town config so `bd prime`, which the formulas tell agents to run themselves, stops dumping 25 KB. Test: prime fixture per role asserts presence or absence of the section and its size. Owner: convoy for gt; operator for the beads config.

### D. Config moves with code

**D1. om install guard.** In the om repo Makefile, `make install` runs `contrib/gastown/deploy.sh --check` for every rig listed in `~/gt/rigs.json` after `go install` and exits non-zero on any drift unless `FORCE=1`. New target `make deploy` runs install, then `deploy.sh --rig <rig> --rubric <rig>/refinery/rig/.om.json --notify` for each rig, then replays `--check`. Closes om-jqq. Owner: convoy on the om rig.

**D2. Rubric-changing MRs.** Directive note today in `gastown/directives/refinery.md`: an MR that touches `.om.json` is gated from a main-content checkout and held until the operator re-stamps. Code: in `editorial.Run` (`review.go:159-173`) resolve head and merge base before `AssertVersion`; if `DiffNameOnly(mergeBase, head)` contains the manifest's rubric path, hash the target's blob via `ShowFile("origin/<target>", path)` instead of the working tree, and run the reviewer against a detached worktree of the target so the MR is reviewed under the current rubric, not its own. In `gt mq post-merge`, when the landed diff touched the rubric, run the rig's re-stamp (`deploy.sh --rig <rig> --rubric <path>`) restricted to the rubric entry and record the new sha on the MR bead; the refinery may write only its own rig's rubric sha. Batch path gets the same detection in `batch_editorial.go:119-131`. Test: a rehearsed MR that edits `.om.json` reviews without `version_mismatch`, and the manifest after post-merge matches the merged file. Owner: operator for the directive; convoy for the code.

**D3. Automatic rebuild, already live; add the two guards it lacks.** The mayor's `make install` at 10:17 on 2026-09-19 re-synced `~/gt/plugins` from the repo, which returned the rebuild-gt plugin to `cooldown 1h`; the daemon ran it in-process at 10:18 (exit 0 in 26 s). Its `run.sh` already checks `gt stale`, refuses a dirty or diverged repo, and installs through `make safe-install`, which renames a temp file over the binary atomically and does not restart the daemon. So the rebuild is automatic and safe for running processes; the condition-gate evaluator in the earlier draft is unnecessary. Two guards remain. First, an in-flight review marker: `editorial.Run` takes a slot-style role `<rig>/om-review` (flock plus owner metadata from `internal/slot`, outside the pool count) for the review's duration, so reviews are visible to `gt slot status` and to the batch path. Second, `run.sh` skips with a recorded reason while any gate-class slot or `om-review` marker is held, because `make build` competes for CPU with a running suite and the suite's tests are load-sensitive; the next cooldown retries. Test: marker held and released on every `editorial.Run` exit path; `run.sh` skip branch driven by a held slot in a temp town. Owner: convoy. Note: any hand edit to `~/gt/plugins` is overwritten by the next `make install` or `gt plugin sync`; parking must be done in the repo or not at all.

**D4. Compaction is monitor-only unless the operator opts in.** Today: `scheduled_maintenance.enabled` false in `mayor/daemon.json` (backup first). Code: a `mode` field (`monitor` default, `flatten`) on `ScheduledMaintenanceConfig` (`scheduled_maintenance.go:33-49`) that escalates with the commit counts instead of running `gt maintain`; the same `monitor` value in `compactorDogMode` (`compactor_dog.go:92-100`); `gt maintain` gains the compactor dog's fetch-and-verify divergence pre-flight (`compactor_dog.go:711-759`) and refuses to flatten a database whose remote has diverged unless `--force-diverged`. Widen gt-nfu7 (in flight, sapphire) to cover all four components under one `compaction.mode` key. Also record the current gt/remote divergence as its own bead: decide whether to push the flattened local over `refs/dolt/data` or restore from the 02:55 backup before the next push. Owner: operator for daemon.json; convoy for the code.

**D5. Witness handoff and the stale formula.** Edit the witness role template `internal/templates/roles/witness.md.tmpl` (source of `gastown/witness/.claude/system-prompt.md:232-255`; the deacon template carries the same block and keeps it, since the deacon hands off every cycle by design) to remove the extraordinary-action and 15-loop triggers; the formula's context-HIGH rule stands and `state.json` carries continuity. Re-render the rig prompt files. Refresh the town-tier `~/gt/.beads/formulas/mol-witness-patrol.formula.toml` from the repo copy (same version 18, 5.5 KB behind) and add a `gt doctor` check that flags a town-tier formula whose version equals the system tier's but whose bytes differ. Owner: operator for the template edit and refresh; convoy for the doctor check.

**D6. om prose tolerance.** In `internal/backend` after `unfence`: if the output is not bare JSON, scan for exactly one top-level JSON object (brace matching outside strings); zero or more than one stays malformed. On extraction, warn on stderr with the byte offsets and persist the raw stdout next to the verdict. Tighten the prompt's final instruction to "output only the JSON object". Tests: fixtures for preamble, trailing chatter, two objects, fenced-plus-preamble. Owner: convoy on the om rig.

**D7. Formula shell interpolation.** One P3 bead: `mol-prd-review:322`, `mol-plan-review:296`, `mol-polecat-code-review:184` put the subject in a shell variable and the body in a quoted heredoc via `--stdin`. Owner: convoy.

## Rollout

Sloan's standing option: the town can be turned off, updated, and turned back on. The ladder is `gt down` (pauses everything, keeps worktrees and branches) for a maintenance window; `gt shutdown` deletes polecat branches and worktrees and is not used for this work.

**Phase 0, by hand, today.** The binary install is done: the mayor ran `make install` at 10:17 (binary 4720428, daemon restarted, plugins re-synced, compactor-dog `run.sh` now the repo's check-only version). The hourly rebuild-gt covers further drift. The daemon restart did not change `scheduled_maintenance`, which is still enabled. Remaining by hand, none of which needs `gt down`: fix the formula Step 2 text in the repo and the town-tier copy; set `batch_min_age` 20m and `batch_max` 6; refresh the town-tier witness formula; edit the witness template and re-render; `scheduled_maintenance.enabled` false; write the dispatch and rubric directives; set `max_local` 3 and the beads `prime.max-memory-chars`; `gt up`. Then watch the first batch run (gt-9z48 records it).

**Phase 1, convoys.** A1 code, A2 epic, A3 code, B1+B3 rule, B2 design comment, C1, C2, C3 gt side, D6, D7, D1. These are independent and can run in parallel across local and flash seats under the new routing.

**Phase 2, convoys after Phase 1 lands.** D2 tooling, D3 rebuild automation, D4 code, A4, D5 doctor check.

**Verification of the whole.** The sampler keeps running with the new columns. Success is read from it after one full night: merges per hour, queue depth, busy-slot histogram, and zero manual interventions in the operator log.

## Tracking

A wayfinder map in the `~/.claude` beads holds the decisions that are still open and the measurements that decide them; build work lives as rig beads (gt-, om-) linked from the map. Open tickets at charting: A5 (gate slots for two refineries, research), B4 (local-model power on the table, research with the sampler), D4's divergence disposition (grilling), and the Phase 2 ordering (task). Existing tickets the map links rather than duplicates: claude-cfv.12 (prime payload), claude-41j.3 (lifecycle authority, which C1 partly answers), claude-yz8.11 (server concurrency knee), claude-mzs and claude-cgy (om reviewer grading).

## Watch items outside this design

- om rig: the deacon reported (mail hq-j8kg1) that polecat jasper's sandbox holds a staged revert of the merged security fix om-y5j (`--tools`/`--restricted` allowlist). The mayor is handling it; this design does not touch om polecat lifecycle. The refinery's editorial gate is the backstop: a revert of a security fix must fail the `security` rubric dimension.
- The dashboard's Merge Queue panel shows GitHub pull requests, not the town's merge-request wisps (workstream E1). Until E1 lands, read the queue with `gt mq list <rig>`.

## Risks

- Batching P1 raises the blast radius of a red batch; `batch_max` 6 and the de-flake epic bound it.
- Routing by type depends on beads being typed honestly; the label override exists for the mayor to correct a misroute.
- The refinery writing its own manifest entry is a new write path into deploy state; it is restricted to one key and logged on the MR bead.
- A condition-gate evaluator that re-fires every heartbeat would loop; the run record plus cooldown is mandatory in the same change.
- The gt/remote Dolt divergence is real now and must be decided before any push, not after.
