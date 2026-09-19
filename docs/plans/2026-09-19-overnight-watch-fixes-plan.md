# Overnight-watch fixes: implementation plan

> **For agentic workers:** This plan is executed by the town. Phase 0 is applied by the operator session by hand. Every other task becomes one rig bead (gt- or om-) with the task text as its description; the polecat that takes it follows mol-polecat-work (TDD, `make lint`, `GOFLAGS=-p=8 make test`, `gt done`). Steps use checkbox syntax for tracking.

**Goal:** Raise merged throughput to three or more MRs per hour, keep three local model slots busy with suitable work, give the mayor and the dashboard process truth, and remove every hand step that config-moves-with-code currently needs.

**Architecture:** Small, independent changes at verified seams in the `gt` binary (refinery batch path, sling pool, polecat list, prime, daemon patrols), the om reviewer, two formulas, three directives, and the web dashboard. No new subsystem. Each task ships with its own tests and lands through the refinery like any other MR.

**Tech Stack:** Go 1.2x (`gt`, `om`), TOML formulas, htmx + vanilla JS dashboard, zsh sampler, beads (`bd`) and Dolt.

**Spec:** `docs/plans/2026-09-19-overnight-watch-fixes-design.md`

## Global Constraints

- No AI attribution anywhere (commits, code, docs).
- Rig gate commands: `make lint`, `GOFLAGS=-p=8 make test`, `make build`. Never bare `go test ./...` at full parallelism on the shared host.
- A check whose failing branch was never exercised is not a check: every new guard gets a test that drives the alarming branch.
- Verify the artifact, never a proxy: a task is done when the merged binary or file behaves, not when the bead closes.
- Editing `gastown/directives/refinery.md`, `scripts/om-gate.sh`, or `formula-overlays/mol-refinery-patrol.toml` changes a sha pinned in `gastown/.gastown-harness-manifest.json` and fails every gate with `version_mismatch` until re-stamped. New directive text goes in new files.
- `gt down` / `gt up` for maintenance windows. Never `gt shutdown`.
- Dolt: never touch `.dolt/` internals; back up before any history operation.

---

## Phase 0: operator, by hand, one window (today)

Owner: this session. Each step has a backup and a verification. Nothing here needs `gt down` except step 0.1 if a gate is running; use the between-gates rule (refinery slot free and no `om` process) or wait.

### Task 0.1: Install the gt binary from main (DONE by the mayor, 2026-09-19 10:17)

The mayor ran `make install`: binary 4720428, daemon restarted at 10:17:24, `~/gt/plugins` re-synced from the repo (rebuild-gt back to `cooldown 1h` and run at 10:18; compactor-dog `run.sh` now the check-only repo version). Further drift is covered hourly by rebuild-gt through `make safe-install` (atomic rename, no daemon restart).

- [x] Binary carries gt-akap. Verify once: `gt patrol state-collapse gastown` prints `MRLookupRan: true` and no findings for queued MRs.

### Task 0.2: Fix the batch-scan formula text and batch config

**Files:**
- Modify: `internal/formula/formulas/mol-refinery-patrol.formula.toml:393-415` (repo, via a docs-class MR or the docs-to-main policy Sloan chooses)
- Modify: `~/gt/.beads/formulas/mol-refinery-patrol.formula.toml` (town-tier copy, same lines; town tier shadows system tier)
- Modify: `~/gt/gastown/config.json` `merge_queue`

- [ ] **Step 1:** Back up: `cp ~/gt/.beads/formulas/mol-refinery-patrol.formula.toml ~/gt/.beads/formulas/mol-refinery-patrol.formula.toml.bak-20260919-batchscan`; `cp ~/gt/gastown/config.json ~/gt/gastown/config.json.bak-20260919-batch`.
- [ ] **Step 2:** Replace Step 2 of the batch-scan step (both files, identical text) with:

```
**Step 2: `gt mq batch run` reviews every candidate itself (T7, gt-2liq, on main since 2026-09-11)**

The batch run rehearses each candidate onto the target, runs the om editorial
review on each one (bounded parallelism), and then stacks only the approved
members. A member that gets request_changes or an infra failure is DROPPED
FROM THE BATCH AND LEFT QUEUED, untouched; nothing is closed and no
FIX_NEEDED is sent from the batch path (the deacon's editorial redispatch
handles it). A member whose patch-id changed once stacked is ejected the same
way. Do NOT pre-review candidates here. On an editorial_required rig the
batch lands only members that hold an approve note, which the run itself
produced.
```

- [ ] **Step 3:** In `gastown/config.json` set `"batch_min_age": "20m"` and `"batch_max": 6` alongside the existing `batch_enabled: true, batch_min_count: 3`.
- [ ] **Step 4:** Verify: `gt mq batch candidates gastown --min-age 20m --max 6 --json | jq .eligible_total` prints a number, and `grep -c "T7 lands" ~/gt/.beads/formulas/mol-refinery-patrol.formula.toml` prints 0.
- [ ] **Step 5:** Commit the repo formula change on `docs/overnight-watch-fixes`; note that until Task 1 lands, P1 MRs are still excluded, so the first batch will be the P2 tail.

### Task 0.3: Compaction monitor-only

**Files:** `~/gt/mayor/daemon.json`

- [ ] **Step 1:** `cp ~/gt/mayor/daemon.json ~/gt/mayor/daemon.json.bak-20260919-maint-off`.
- [ ] **Step 2:** Set `patrols.scheduled_maintenance.enabled` to `false`. Leave `compactor_dog` as is (it has never fired; Task 15 gives it a mode).
- [ ] **Step 3:** Daemon reads config at start: `launchctl kickstart -k gui/501/com.gastown.daemon` only in a between-gates gap (the daemon restart drops nothing but the heartbeat). Verify with `grep -a "Scheduled maintenance ticker" ~/gt/daemon/daemon.log | tail -1` absent after the restart timestamp, or the log line saying the patrol is disabled.
- [ ] **Step 4:** File bead "Dolt gt database diverged from refs/dolt/data after nightly flatten; decide push-over vs restore before the next push" (P1, hq) linking the 02:55 backup. Do not push either way today.

### Task 0.4: Witness handoff rule and stale town-tier formula

**Files:**
- Modify: `~/gt/gastown/witness/.claude/system-prompt.md:232-255` (rendered; Task 17 fixes the template)
- Replace: `~/gt/.beads/formulas/mol-witness-patrol.formula.toml` with `internal/formula/formulas/mol-witness-patrol.formula.toml` from main
- Modify: `~/gt/gastown/witness/state.json` (`extraordinary_action` false)

- [ ] **Step 1:** Back up both files with a `.bak-20260919-*` suffix.
- [ ] **Step 2:** In the rendered prompt, replace the "Context Management" heuristic with: hand off only when `context-check` reports HIGH; otherwise `gt patrol report` and loop. Delete the "Extraordinary actions" list and the `patrol_count >= 15` rule.
- [ ] **Step 3:** Copy the repo formula over the town-tier copy; `diff` must be empty afterwards; `grep -c "Stall Judgement" ~/gt/.beads/formulas/mol-witness-patrol.formula.toml` prints at least 1.
- [ ] **Step 4:** The running witness keeps its old prompt until its next handoff; do not restart it. Verify on the next witness session that its transcript never calls `gt handoff` after an escalation.

### Task 0.5: Directives

**Files:**
- Create: `~/gt/gastown/directives/dispatch.md`
- Create: `~/gt/gastown/directives/rubric-mrs.md`
- Create the same two under `~/gt/om/directives/`

`refinery.md` is pinned by the harness manifest; do not edit it.

- [ ] **Step 1:** `dispatch.md`:

```
## Dispatch backpressure

> **Rig Policy — overrides formula instructions where they conflict.**

- Do not sling new work while `gt mq list <rig>` shows more than 12 ready MRs.
  Exceptions: rework of a rejected MR (bead carries label `rework`), and
  rubric or config MRs.
- Route by shape until the pool does it: `--agent local-coder-polecat` for
  type task/chore/docs and for rework that carries om findings; the pool
  default (overflow) for bug/feature logic.
- Before acting on any STATE_COLLAPSE mail, check `gt mq list <rig>` for an
  open MR on the same issue or branch.
```

- [ ] **Step 2:** `rubric-mrs.md`:

```
## Rubric-changing merge requests

> **Rig Policy — overrides formula instructions where they conflict.**

An MR whose diff touches `.om.json` cannot be gated from this refinery's own
checkout: the harness manifest pins the rubric sha and AssertVersion hashes
the working tree. Gate it from a detached worktree of origin/<target>
(`git worktree add --detach /tmp/gt-rubric-gate origin/main`) and run
`gt mq review <mr> --rehearsed <sha>` with that worktree as cwd. After the
merge, HOLD further gates and mail mayor/ "RUBRIC MERGED <mr>: re-stamp
needed"; resume when the manifest sha equals the merged file's sha.
```

- [ ] **Step 3:** Verify `gt prime --dry-run` from `~/gt/gastown/refinery/rig` lists both directives in its Directives section.

### Task 0.6: Pool and memory config

**Files:** `~/gt/settings/config.json` (`polecat_pool.max_local`), beads config.

- [ ] **Step 1:** `cp ~/gt/settings/config.json ~/gt/settings/config.json.bak-20260919-pool3`; set `polecat_pool.max_local` to 3. No restart needed; the next sling reads it.
- [ ] **Step 2:** `bd config set prime.max-memory-chars 4000` and `bd config set prime.max-memories 12` from `~/gt` (the keys `primeMemoryCaps()` reads in `beads cmd/bd/prime.go:499-516`). Verify `bd prime | wc -c` drops below 12000.
- [ ] **Step 3:** Record both in hq-5n25a.

### Task 0.7: Sampler GPU column

**Files:** `~/.claude/tools/town-stats.sh`

- [ ] **Step 1:** Add after the `lrss=` line: `gpu=$(ioreg -r -d 1 -c IOAccelerator 2>/dev/null | grep -o '"Device Utilization %"=[0-9]*' | head -1 | grep -oE '[0-9]+$')` and append `\t${gpu:-?}` to the echo and `\tgpu` to the header line.
- [ ] **Step 2:** Start a second sampler into a new dir (`throughput-20260919`) with the current origin/main as base rather than editing the running one; keep the old one running until midnight for continuity, then stop it by pid.
- [ ] **Step 3:** Verify two rows land with a numeric `gpu`.

---

## Phase 1: convoys (gastown rig unless stated)

Each task below is one bead. Bead priority P1 unless stated. Dependencies are listed; wire them with `bd dep add`.

### Task 1: Batch P1 and P2; enforce batch_min_count in code (A1)

**Files:**
- Modify: `internal/cmd/mq_batch.go:149-172` (filter), `:275-407` (run)
- Modify: `internal/config/types.go:1675` doc comment
- Test: `internal/cmd/mq_batch_test.go`

**Interfaces:**
- Produces: `filterAndSortBatchCandidates` excludes only `Priority == 0`; `runMQBatchRun` returns nil after printing `batch: N eligible < min_count M, not batching` when below threshold; `--min-count` flag mirrors `mq.GetBatchMinCount()`.

- [ ] **Step 1:** Failing test: candidates with priorities 0,1,2 and age 2h, minAge 1h; expect ids of P1 and P2 in output, P0 absent.
- [ ] **Step 2:** Run `go test ./internal/cmd -run TestFilterAndSortBatchCandidates -count=1`; expect FAIL (P1 missing).
- [ ] **Step 3:** Change `mq_batch.go:152` to `if mr.Priority == 0 { continue }` and update the comment to "P0: always single-MR, never batched (P1 batches since 2026-09-19)".
- [ ] **Step 4:** Failing test for min-count: fake engine with 2 eligible, min-count 3; expect no `ProcessBatch` call and the stdout line above.
- [ ] **Step 5:** Implement: in `runMQBatchRun` after assembling candidates and before the slot acquire, `if len(candidates) < minCount { fmt.Printf(...); return nil }` with `minCount` from the new `--min-count` flag defaulting to `mq.GetBatchMinCount()`.
- [ ] **Step 6:** Update the formula var table line for `batch_min_count` to say the run enforces it too. Run the package tests; `make lint`; commit `feat(mq): batch P1 MRs and enforce batch_min_count in gt mq batch run (A1)`.

Acceptance: first live batch run after merge lands two or more P1 MRs in one suite (gt-9z48 records it).

### Task 2: De-flake epic (A2)

One epic bead `[epic] De-flake the gate suite: per-package fixes for load-sensitive tests` with children, each P1, each listing the existing beads it absorbs:

| Child | Package | Absorbs |
|---|---|---|
| 2a | internal/cmd | gt-v2a5, gt-5clb, gt-fhkg, gt-ul8f |
| 2b | internal/refinery | gt-kyct, gt-bzkt (the batch_editorial half) |
| 2c | internal/config | gt-5v82, gt-bzkt (dolt socket half) |
| 2d | internal/polecat | gt-nyh8, gt-e1u5 |
| 2e | internal/nudge | gt-v25y, gt-38ss, gt-k9sb |
| 2f | internal/tmux | gt-rdvq, gt-xt7o, gt-n0jv |
| 2g | internal/mayor + lint lock | gt-toc1, gt-taoz |

Each child's description ends with the same acceptance block:

```
Acceptance: each named test passes `go test ./<pkg> -run '<names>' -count=10`
while `GOFLAGS=-p=8 make test` runs concurrently on the same host; the fix
asserts a bound or uses a barrier/fake clock, never a longer sleep; the
absorbed beads are closed with reason "fixed-by: <this bead>".
```

Route: flash (label `route:flash`).

### Task 3: Sling backpressure (A3)

**Files:**
- Modify: `internal/config/types.go:1491-1508` (add `MaxReadyForDispatch int \`json:"max_ready_for_dispatch,omitempty"\`` + accessor `GetMaxReadyForDispatch() int`)
- Modify: `internal/cmd/polecat_spawn.go:130-138` (guard before pool decision)
- Modify: `internal/daemon/convoy_manager.go` feed path (defer when over threshold)
- Test: `internal/cmd/sling_backpressure_test.go`, `internal/daemon/convoy_manager_test.go`

**Interfaces:**
- Produces: `errQueueBackpressure` (typed error, message `sling refused: <rig> has N ready MRs (> M); pass --force or label the bead rework`); `SlingOptions.Force bool`.
- Consumes: `beads.ListMergeRequests` for the ready count (bulk, wisps-aware).

- [ ] **Step 1:** Failing test: fake MR lister returns 13 open MRs, config max 12, bead without `rework` label; expect `errQueueBackpressure`.
- [ ] **Step 2:** Failing test: same with label `rework`; expect nil. Same with `--force`; expect nil. Config 0; expect nil with no lister call.
- [ ] **Step 3:** Implement in `SpawnPolecatForSling` before `resolvePolecatPoolAgent`.
- [ ] **Step 4:** Convoy feeder: when the guard refuses, log `convoy <id>: deferring <bead>: <reason>` and leave the bead ready; test that the next tick re-offers it and that the bead's status is unchanged.
- [ ] **Step 5:** Tests, lint, commit `feat(sling): refuse new dispatch above merge_queue.max_ready_for_dispatch (A3)`.

Acceptance: with the knob set to 12 in `gastown/config.json`, `gt sling <bead> gastown` refuses at 13 ready MRs and the daemon log shows deferral, not failure.

### Task 4: Route by bead shape, unambiguous pool line, local-attempt rule (B1, B3)

**Files:**
- Modify: `internal/cmd/sling_pool.go` (whole file; keep `choosePoolAgent` pure)
- Modify: `internal/cmd/polecat_spawn.go:138-143`
- Test: `internal/cmd/sling_pool_test.go`

**Interfaces:**
- Produces: `type poolBead struct { ID string; Type string; Labels []string }`; `choosePoolAgent(pool *config.PolecatPool, bead poolBead, sessions []poolSession, now time.Time) (agent, reason string)`; `resolvePolecatPoolAgent(townRoot, beadID string)`.
- Reason strings (exact):
  - `pool: local seat %d/%d -> %s (%s)` where the parenthetical is `label route:local`, `type=task`, `rework`, or `idle-seat fill, local-attempt:1`
  - `pool: overflow -> %s (%s)` with `label route:flash`, `type=bug`, `type=feature`, or `local-attempt:1 failed`
  - `pool: local full (%d/%d) -> %s`
  - `pool: stagger %s since last local spawn -> %s`

Decision order: label `route:local`/`route:flash` wins; then a bead carrying `local-attempt:1` whose previous MR was rejected or stalled goes overflow; then type task/chore/docs or label `rework` goes local if a seat is free; then bug/feature goes overflow unless a seat has been free longer than `min_spawn_gap`, in which case it goes local and the sling adds label `local-attempt:1`; then the existing seat count and stagger.

- [ ] **Step 1:** Table test covering the eight cases above, asserting both agent and exact reason string.
- [ ] **Step 2:** Implement; thread the bead from `opts.HookBead` through `bd show --json` (type, labels) in `resolvePolecatPoolAgent`; on lookup error fall back to the current seat-only decision and say so in the reason.
- [ ] **Step 3:** The sling adds `local-attempt:1` via `bd label add` when the idle-seat fill branch fires; test with a fake bd.
- [ ] **Step 4:** Tests, lint, commit `feat(sling): route polecats by bead shape and always print the chosen agent (B1, B3; closes gt-ipk7)`.

Acceptance: `gt sling --dry-run` prints one of the four lines for a task-type bead and a bug-type bead respectively.

### Task 5: Design comment on gt-eoi9 (B2)

Not a bead: `bd comments add gt-eoi9` with: reservation must carry the agent; merge reservations into `listPolecatSessions` as synthetic sessions `{name: "pending:<polecat>", agent, created}`; expire after 5m; read `agent_state=spawning` beads and add an `agent` field to `AgentFields` written at spawn. Done in Phase 0 by the operator.

### Task 6: `gt polecat list` agent and MR columns, spawn grace (C1)

**Files:**
- Modify: `internal/cmd/polecat_inventory.go:37-47` (session set carries agent + created), `:76-139` (item fields), `:116` (grace)
- Modify: `internal/cmd/polecat.go:395-418` (JSON fields `agent`, `mr_id`, `mr_status`), `:434` (grace), `:517` (bulk MR join), `:612-660` (columns)
- Modify: `internal/polecat/manager.go:2996` (grace)
- Test: `internal/cmd/polecat_inventory_test.go`, `internal/cmd/polecat_list_test.go`

**Interfaces:**
- Produces: `PolecatListItem.Agent string`, `.MRID string`, `.MRStatus string` (`open`, `ready`, `blocked`, `merged`, `rejected`, `missing`); `polecatSessionSet` entries `{Name, Agent string; Created time.Time}`; `spawnGrace(agentState string, updatedAt time.Time, now time.Time, grace time.Duration) bool`.
- Consumes: `beads.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "", Rig: rig, Priority: -1})` once; join on `MRFields.Worker` then `AgentFields.ActiveMR`.

- [ ] **Step 1:** Failing test: an agent bead with `active_mr` set and a fake MR lister returning that MR as open with no blockers; expect `MRStatus == "ready"`. Same with the MR absent; expect `missing`.
- [ ] **Step 2:** Failing test: hooked bead, no session, agent bead `spawning` updated 30s ago; expect state `spawning`, not `stalled`. Same at 6m; expect `stalled`.
- [ ] **Step 3:** Implement the three grace sites and the bulk join; add `AGENT` and `MR` columns to the text output; keep `--json` backward compatible (additive fields).
- [ ] **Step 4:** Tests, lint, commit `feat(polecat): list shows agent and MR state; spawn grace before stalled (C1; closes gt-yteq, gt-aj7 partial)`.

Acceptance: `gt polecat list gastown --json | jq '.[0] | {agent, mr_id, mr_status}'` returns the running agent and a real MR status for a polecat with a queued MR.

### Task 7: DetectStateCollapse must see MR wisps (C2)

**Files:**
- Modify: `internal/witness/state_collapse.go:60-110` (inject `ListOpenMRs`, remove `bd list --label`)
- Modify: `internal/cmd/patrol_state_collapse.go:140-175` (wire the same source; zero result wording)
- Test: `internal/witness/state_collapse_test.go`

- [ ] **Step 1:** Failing test: source returns 2 open MRs whose source issues are closed; expect 2 collapse findings. Source returns error; expect `MRLookupRan == false` and output containing `lookup unavailable`.
- [ ] **Step 2:** Implement; the CLI prints `checked N open MRs (lookup ran)` and never "no state collapse found" when N is 0 without a successful lookup.
- [ ] **Step 3:** Re-scope gt-ke09 to this bead (`bd update gt-ke09 --title ...`) or close it as absorbed. Commit `fix(witness): DetectStateCollapse reads MR wisps via ListMergeRequests (C2)`.

### Task 8: Memories in prime: role gate, cap, namespace (C3)

**Files:**
- Modify: `internal/cmd/prime.go:292`, `:632-639`, `:654-661` (role gate), `:695-707`
- Modify: `internal/cmd/memory_index.go:28` (3000), `:46` (accept `gt.` and `memory.` prefixes)
- Test: `internal/cmd/memory_index_test.go`, `internal/cmd/prime_payload_test.go`

**Interfaces:**
- Produces: `shouldRenderMemories(role string) bool` true for `mayor`, `crew`; `memoryInjectMaxChars = 3000`; `collectMemories` groups `memory.<key>` as type `memory`.

- [ ] **Step 1:** Failing test: prime fixture for role witness contains no `# Agent Memories`; for mayor it does and its length is under 3000.
- [ ] **Step 2:** Failing test: `collectMemories` over kv rows with keys `gt.feedback.x` and `memory.y` returns both.
- [ ] **Step 3:** Implement; commit `fix(prime): memories only for mayor and crew, capped to fit the hook budget, both key namespaces (C3)`.

### Task 9: Formula shell interpolation (D7), P3

**Files:** `internal/formula/formulas/mol-prd-review.formula.toml:322`, `mol-plan-review.formula.toml:296`, `mol-polecat-code-review.formula.toml:184`; town-tier copies refreshed by the operator after merge.

- [ ] **Step 1:** Rewrite each `gt mail send` as: subject from a shell variable read via `bd show <bead> --json | jq -r .title`, body via `--stdin <<'BODY' ... BODY`. No template var inside a double-quoted shell string.
- [ ] **Step 2:** Add a formula test asserting no line matches `-s "[^"]*\{\{` in any shipped formula. Commit `fix(formulas): stop interpolating template vars into shell strings (D7)`.

### Task 10: Dashboard truth panels (E)

Four beads on the gastown rig, all touching `internal/web`. Standing operator bead hq-6sbi7 tracks verification.

**10a. Merge Queue panel shows the town's MRs, not GitHub PRs.** `FetchMergeQueue` (`fetcher.go:649-680`) fetches PRs per repo. Add `FetchTownMergeQueue()` using `beads.ListMergeRequests` per rig (the `gt mq list` derivation at `mq_list.go:199-210` for ready/blocked), columns `ID PRI RIG BRANCH STATUS AGE ASSIGNEE`; keep the PR list as a second tab. Test: fetcher unit test with a fake lister. Panel header shows `N ready` and a throughput tile `merges last 6h: N` from `git rev-list --count --since=6h --merges origin/main` per rig (cached 5 min).

**10b. Local pool panel.** `FetchLocalPool()` reads `http://127.0.0.1:8099/slots` (3 rows: id, busy/idle, context tokens) and lists polecat sessions whose `GT_AGENT` is the local agent, plus the pool config (`max_local`, `min_spawn_gap`). Tile: `local seats 2/3, slots busy 1/3`. Timeout 2s, circuit breaker as the other fetchers. Test with a stub HTTP server.

**10c. Gate panel.** `FetchGate()` wraps `slot.StatusPool` (holders, roles, ages) plus the `om-review` markers once Task 14a lands; shows `refinery: gating <mr> since 12m` and `batch run: 5 members` when the batch role holds a slot. Test with a fake pool report.

**10d. Polecats panel uses the C1 fields.** Add `AGENT` and `MR` columns from `gt polecat list --json` (Task 6). Replace the hardcoded `status=unknown`. Depends on Task 6.

Each bead ends with: `Acceptance: panel renders in the live dashboard at http://127.0.0.1:8080 with real data; verified via cmux browser get text; no new bd process fan-out per poll (count bd processes before and after with ps).`

### Task 11: om install guard and deploy target (D1), om rig

**Files:** `Makefile` (om repo), `contrib/gastown/deploy.sh` (accept `--all-rigs` reading `~/gt/rigs.json`), `contrib/gastown/deploy_test.sh`.

- [ ] **Step 1:** `make install` = `go install ./cmd/om` then `deploy.sh --check --all-rigs`; non-zero on drift unless `FORCE=1`. Test: a temp rig dir with a stale manifest makes `make install` exit 1 and print the drifted artifact; `FORCE=1` exits 0.
- [ ] **Step 2:** `make deploy` = install, then `deploy.sh --rig <r> --rubric <r>/refinery/rig/.om.json --notify` for each rig, then `--check --all-rigs`. Test in the same temp layout.
- [ ] **Step 3:** Commit `build: make install refuses on manifest drift; make deploy re-stamps every rig (om-jqq)`.

### Task 12: om prose tolerance (D6), om rig

**Files:** `internal/backend/backend.go` (after `unfence`), `internal/backend/extract.go` (new: `extractSingleJSONObject(b []byte) (obj []byte, offsets [2]int, ok bool)`), `internal/backend/backend_test.go`, prompt template final instruction.

- [ ] **Step 1:** Failing tests: `preamble` (`Here is my verdict:\n{...}`) parses; `trailing` (`{...}\nLet me know`) parses; `two-objects` fails malformed; `unterminated` fails; nested braces inside strings handled.
- [ ] **Step 2:** Implement brace matching that skips string literals and escapes; on extraction write `stderr: om: extracted verdict object at bytes [a,b) from prose output` and persist raw stdout to `<tmpdir>/raw-stdout.txt`.
- [ ] **Step 3:** Tighten the prompt's last line to `Output only the JSON object. No prose before or after it.` Commit `fix(backend): accept exactly one JSON object surrounded by prose, warn and persist raw (om-was)`.

After merge the operator runs `make deploy` (Task 11) so every rig's manifest carries the new binary.

---

## Phase 2: convoys after Phase 1

### Task 13: Rubric-changing MRs gated and re-stamped by tooling (D2)

**Files:**
- Modify: `internal/refinery/editorial/review.go:159-199` (order: resolve head and merge base, then detect, then assert)
- Modify: `internal/refinery/editorial/manifest.go:66-135` (`AssertVersion(..., rubricBlob []byte)` variant)
- Modify: `internal/refinery/batch_editorial.go:119-131`
- Modify: `internal/cmd/mq.go:181` (`post-merge` command; add the re-stamp step)
- Test: `internal/refinery/editorial/review_rubric_test.go`

**Interfaces:**
- Produces: `rubricTouched(files []string, rubricPath string) bool`; `AssertVersionWithRubric(m, cfg, rigDir, rubric []byte)`; post-merge `restampRubric(rigDir, rubricPath string, sha string) error` writing only `manifest.Rubric.SHA256`.

- [ ] **Step 1:** Failing test: rehearsed head whose diff includes `.om.json`; worktree rubric differs from manifest; target blob matches; expect no `VersionMismatch` and the review runs against a detached worktree of the target.
- [ ] **Step 2:** Failing test: post-merge on a landed diff that touched `.om.json` updates the manifest rubric sha to the merged blob's sha and records `rubric_restamped: <sha>` on the MR bead; a diff without it leaves the manifest untouched.
- [ ] **Step 3:** Implement; commit `feat(refinery): gate rubric-changing MRs against the target rubric and re-stamp after merge (D2)`.

### Task 14: Rebuild guards (D3), two beads

Automatic rebuild is already live (cooldown 1h, atomic `make safe-install`); no gate evaluator is needed.

**14a. In-flight review marker.** `editorial.Run` acquires slot role `<rig>/om-review` (flock + owner file via `internal/slot`, outside the pool count) for the review's duration; `gt slot status` lists it. Test: a fake review holds the marker; status shows it; release on every exit path (defer), including `AssertVersion` failure.

**14b. rebuild-gt yields to a running gate.** In `plugins/rebuild-gt/run.sh`, before `make build`: if `gt slot status --json` reports any holder whose role ends in `/refinery`, `/refinery-batch`, `/main-branch-test`, or `/om-review`, record a `skipped` run with title `Plugin: rebuild-gt [skipped: gate busy <role>]` and exit 0. Test: `plugins/rebuild-gt/run_test.sh` with a stubbed `gt` on PATH returning a held slot drives the skip branch; a free pool drives the build branch (stub `make`). Depends on 14a for the om-review role name only; ships without it.

### Task 18: Sling step timing and the dominant step (A6)

**Files:**
- Modify: `internal/cmd/polecat_spawn.go` (`SpawnPolecatForSling`), `internal/cmd/sling.go:1100-1140` (session start), `internal/cmd/sling_formula.go` (cook/bond), `internal/daemon/convoy_manager.go` (feed)
- Test: `internal/cmd/sling_timing_test.go`

**Interfaces:**
- Produces: `type slingTimer struct{ start time.Time; last time.Time; w io.Writer }`, `func (t *slingTimer) step(name string)` printing `[sling] step %s took %s (total %s)`; `--json` output gains `"timings": {"admission": ms, "allocate": ms, "worktree": ms, "convoy": ms, "formula": ms, "hook": ms, "session": ms}`.

- [ ] **Step 1:** Failing test: a fake spawn with two steps prints two `[sling] step` lines in order with non-negative durations and the total equals their sum within 1 ms.
- [ ] **Step 2:** Implement the timer and call `step()` after admission, allocation, worktree verification, convoy creation, formula cook and bond, hook write, and `StartSession`.
- [ ] **Step 3:** Commit `feat(sling): per-step timing on the dispatch path (A6)`. After merge the mayor's next three slings give the dominant step; the map's research ticket records it and files the fix bead.

Measured baseline to beat: gt-ipk7 fed 10:20:19, session created 10:31:34 (11m15s). Dry-run 1.3 s; `bd show` 0.26 s; host load 36-44 at the time.

### Task 15: Compaction mode and gt maintain pre-flight (D4)

**Files:**
- Modify: `internal/daemon/scheduled_maintenance.go:33-49`, `:204-214`
- Modify: `internal/daemon/compactor_dog.go:92-100`, `:192-198`
- Modify: `internal/cmd/maintain.go:63-68` (flags), `:300-420` (pre-flight before flatten)
- Modify: `internal/cmd/config.go:914-982`, `:1062-1090` (`maintenance.mode`, `lifecycle.compactor.mode`)
- Test: `internal/daemon/scheduled_maintenance_test.go`, `internal/cmd/maintain_test.go`

Widen gt-nfu7 (in flight) to this scope by comment; if its MR lands first, this task is the delta.

- [ ] **Step 1:** Failing test: mode `monitor` with a DB over threshold calls `escalate` with the counts and never execs `gt maintain`.
- [ ] **Step 2:** Failing test: `gt maintain` on a DB whose remote HEAD is not in local `dolt_log` refuses with `diverged from <remote>; pass --force-diverged` and flattens nothing.
- [ ] **Step 3:** Implement with the `compactorFetchAndVerify` logic moved to a shared helper in `internal/dolt`. Default mode `monitor` in both daemons. Commit `feat(daemon): compaction is monitor-only by default; gt maintain refuses to flatten over a diverged remote (D4)`.

### Task 16: main_branch_test skips when the gate is busy (A4, gt-lf2r)

**Files:** `internal/daemon/main_branch_test_runner.go:224-266`, `:269-366`, `:88-101`; `lifecycle_defaults.go:61-65`; test file alongside.

- [ ] **Step 1:** Failing test: pool report shows `gastown/refinery` holding slot 0; expect the rig skipped with reason `gate busy: gastown/refinery` before any fetch, and the cycle summary `0 tested, 1 skipped`.
- [ ] **Step 2:** Implement with `skip_when_gate_busy` (default true) in `MainBranchTestConfig`; the summary line distinguishes skipped from tested. Commit `feat(daemon): main_branch_test yields to a busy gate (gt-lf2r)`.

### Task 17: Witness template and stale-formula doctor check (D5)

**Files:** `internal/templates/roles/witness.md.tmpl` (remove the extraordinary-action and 15-loop rules); `internal/doctor/formula_tier_check.go` (new); test.

- [ ] **Step 1:** Failing doctor test: town-tier formula with the same `version` as the system tier but different bytes yields a warning naming the file and both sizes.
- [ ] **Step 2:** Implement; template edit; commit `fix(witness): hand off on context only; doctor flags stale same-version formula overrides (D5)`.

---

## Wayfinder map (~/.claude beads)

Map: `Gas Town after the overnight watch: throughput, local-slot utilisation, process truth, config-moves-with-code (wayfinder map)`, label `wayfinder:map`, Notes: execution is in the map (rig beads linked by id), sampler location, standing rules (operator watches, mayor manages). Decisions so far: the fourteen round-1 answers, gisted, linking hq-5n25a.

Tickets (children):

1. `research` Gate slots for two refineries: measure peak Docker VM memory of one gastown suite and one om suite under the slot; decide `reserved_for_gate` (blocks nothing; informs A5).
6. `research` Sling dispatch latency: with Task 18's timings from three real slings, name the dominant step and file its fix bead (blocked by Task 18).
2. `research` Local-model power on the table: from the sampler with the gpu column, is decode or prefill the bottleneck at three busy seats, and does `--metrics` change any decision (blocked by Task 0.7).
3. `grilling` Dolt gt divergence disposition: push the flattened local over refs/dolt/data, or restore from the 02:55 backup (blocks any push).
4. `task` Phase 2 ordering and the maintenance window for the rebuild-gt un-park (blocked by Phase 1 beads).
5. `task` Dashboard panels beyond E1-E4: which of pool, gate, throughput, compaction status the operator actually reads (links hq-6sbi7).

Existing tickets linked, not duplicated: claude-cfv.12, claude-41j.3, claude-41j.4, claude-yz8.11, claude-mzs, claude-cgy, claude-3ah.

## Dependencies

- Task 10d depends on Task 6. Task 10c's om-review rows depend on Task 14a.
- Task 14b depends on 14a; 14c depends on 14b.
- Task 13 depends on nothing but should land after Task 1 so the first rubric MR is not batched.
- Task 15 coordinates with gt-nfu7.
- Phase 0.2 must precede Task 1's first live batch (formula text) and Task 0.1 precedes 0.2 (binary carries the batch fixes already on main).

## Self-review

Spec coverage: A1 (0.2, 1), A2 (2), A3 (0.5, 3), A4 (16), A5 (map 1), A6 (18, map 6), B1 (4), B2 (5), B3 (0.6, 4), B4 (0.7, map 2), C1 (6), C2 (7), C3 (0.6, 8), D1 (11), D2 (0.5, 13), D3 (14), D4 (0.3, 15, map 3), D5 (0.4, 17), D6 (12), D7 (9), E (10, map 5). No placeholders. Names used across tasks: `PolecatListItem.Agent/MRID/MRStatus` (6, 10d), `<rig>/om-review` role (14a, 14b, 10c), `errQueueBackpressure` (3), `poolBead` (4).
