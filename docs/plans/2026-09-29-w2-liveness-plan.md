# Liveness stopgaps (gt-fcxe9.1, gt-fcxe9.7) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** No destructive path acts on an unknown liveness or agent-bead answer: tmux query errors, `git rev-parse` failures and agent-bead misses are reported, never treated as death.

**Architecture:** Delete `tmux.IsAgentAlive` (the error-dropping form) and move every caller to `IsAgentAliveChecked`, choosing per caller what an error means (destructive callers skip and report). Replace the `.repo.git` RemoveAll with a quarantine rename that runs only when HEAD is missing and the repo is provably unneeded. Point the daemon's agent-bead reads at the canonical database (the same prefix resolver `beads.ForAgentBead` uses), make its not-found path a logged UNKNOWN, and delete the witness branch that restarts a live polecat on the unmaintained `hook_bead` slot.

**Tech Stack:** Go, tmux, bd CLI (faked by shell scripts in daemon tests).

**Spec:** beads gt-fcxe9.1 and gt-fcxe9.7; evidence in `~/.claude/docs/research/deep-review/gastown-cli-operator.md` (G4-01, G4-02) and `gastown-control-plane.md` (G1-01, G1-03); direction in `docs/adr/0003-one-supervisor-no-idle-llm.md` ("Unknown is never acted on"; the witness loses restart authority).

## Global Constraints

- Stopgap only: no new liveness type, no supervisor, no intent file (those are the ADR 0003 refactor).
- Do not touch `internal/beads` (another builder owns not-found routing there) or formula lint.
- Unknown is never acted on: every error path in a kill/restart/respawn/send-keys decision skips and reports.
- Tests first; each fix gets a test that fails on origin/main.
- No AI attribution in commits.

---

### Task 1: Delete IsAgentAlive; callers use the checked form (gt-fcxe9.1, G4-01)

**Files:**
- Modify: `internal/tmux/tmux.go` (delete `IsAgentAlive`; `CheckSessionHealth`, `CleanupOrphanedSessions`)
- Modify: `internal/doctor/zombie_check.go` (+ test fake)
- Modify: `internal/cmd/mayor.go`, `internal/cmd/start.go`, `internal/cmd/crew_at.go`, `internal/cmd/status.go`
- Modify: `internal/polecat/session_manager.go`, `internal/crew/manager.go`, `internal/deacon/manager.go` (+ mock), `internal/session/lifecycle.go`
- Modify: `internal/witness/handlers.go`, `internal/witness/composer_stall.go`, `internal/witness/realactivity.go`, `internal/witness/manager.go`
- Modify: `internal/daemon/tmux_seam.go`, `internal/daemon/lifecycle.go`, `internal/daemon/daemon.go`, `internal/daemon/patrol_watchdog.go` (+ `tmux_fake_test.go`)
- Test: `internal/doctor/zombie_check_test.go`, `internal/cmd/start_liveness_test.go` or the nearest existing seam

**Interfaces:**
- Produces: no `IsAgentAlive` anywhere; seams (`zombieSessionLister`, daemon `sessionTmux`, deacon tmux interface) declare `IsAgentAliveChecked(session string) (bool, error)`.
- `tmux.CheckSessionHealth` returns a new `ZombieStatus` value `SessionHealthUnknown` when the agent query errors; callers that act on `AgentDead` do not act on it.

Per-caller policy on a liveness-query error:

| Caller | Acts on "dead" by | On error |
|---|---|---|
| doctor zombie Run/Fix | KillSessionWithProcesses | not listed as zombie; reported as unknown; Fix re-check skips |
| `gt mayor attach` | KillPaneProcesses + RespawnPane | return error, no respawn |
| `gt start` crew | SendKeys startup command | "liveness unknown, left alone" message |
| `gt crew at` (restart branch, in-session branch) | RespawnPane / exec agent | return error |
| polecat/crew/deacon/witness `Start` | kill + recreate | return error (session left as is) |
| session.KillExistingSession(checkAlive) | kill | return error |
| tmux.CleanupOrphanedSessions | kill | skip, warn |
| witness zombie live-session check | restart | not a zombie this cycle, logged |
| witness stall/composer checks, realactivity | nothing destructive | skip / record error |
| daemon GUPP, orphaned work, idle reaper, mayor zombie | mail / kill / restart | treated as alive (no action), logged |
| daemon patrol watchdog, `gt status` | display/alert | treated as alive (unknown is not dead) |

- [ ] **Step 1: failing tests.** Zombie check: fake lister whose `IsAgentAliveChecked` returns `(false, errors.New("tmux show-environment: timeout"))` for one session; `Run` must not list it as a zombie and `Fix` must kill nothing. Daemon fake tmux: a liveness error must not produce a kill in the idle reaper. Start path: an erroring liveness probe must not send keys (test through a small seam in `startOrRestartCrewMember`).
- [ ] **Step 2: run, expect FAIL** (`go test ./internal/doctor/ -run Zombie`), and compile failures where fakes still implement the old method.
- [ ] **Step 3: implement** per the table; delete `IsAgentAlive`; update fakes and the comments that name it.
- [ ] **Step 4: run** `go build ./... && go test ./internal/tmux/ ./internal/doctor/ ./internal/cmd/ ./internal/witness/ ./internal/daemon/ ./internal/deacon/ ./internal/polecat/ ./internal/crew/ ./internal/session/`.
- [ ] **Step 5: commit** `fix(liveness): act only on confirmed-dead agent sessions; delete IsAgentAlive`.

### Task 2: doctor never deletes a .repo.git it cannot prove unneeded (gt-fcxe9.1, G4-02)

**Files:**
- Modify: `internal/doctor/rig_check.go` (`bareRepoHealth` → classified result; `BareRepoExistsCheck.Run/Fix`)
- Test: `internal/doctor/bare_repo_exists_check_test.go`

**Interfaces:**
- `bareRepoHealth(path) error` keeps its signature for `BareRepoRefspecCheck`; new `classifyBareRepo(path) bareRepoState` returns `bareRepoHealthy`, `bareRepoCorrupt` (HEAD missing: the recurring objects/+worktrees/ shell), or `bareRepoUnverified` (any `git rev-parse` failure, or a non-bare repo).
- Fix quarantines only `bareRepoCorrupt` repos with no referencing worktree, no `worktrees/*` entry and no local branch ref that is not identical to `refs/remotes/origin/<same>`; it renames to `.repo.git.corrupt-<unix>` instead of RemoveAll, then re-clones.

- [ ] **Step 1: failing tests.** (a) PATH points at a fake `git` that exits 1 for everything; HEAD present; Run must report an unverified error and Fix must leave `.repo.git` in place with its objects. (b) Corrupt shell with a registered worktree: Fix returns an error naming the worktree and leaves `.repo.git` in place. (c) Corrupt shell with a local branch ref not on origin: Fix refuses. (d) Corrupt shell, no references: Fix re-clones and a `.repo.git.corrupt-*` directory exists holding the old objects.
- [ ] **Step 2: run, expect FAIL.**
- [ ] **Step 3: implement**; drop `recoveredHeads` (the corrupt path no longer re-registers worktrees because it refuses when any exist); rework `FixCorruptPreservesWorktreeHead` into the refusal test.
- [ ] **Step 4: run** `go test ./internal/doctor/`.
- [ ] **Step 5: commit** `fix(doctor): quarantine .repo.git only when provably unneeded; never on rev-parse error`.

### Task 3: daemon reads agent beads from their canonical DB; not-found is UNKNOWN (gt-fcxe9.7, G1-01)

**Files:**
- Modify: `internal/daemon/lifecycle.go` (`getAgentBeadInfo`, `getAgentHookBead`, `listAgentBeadsJSON`)
- Modify: `internal/daemon/daemon.go` (`checkPolecatHealth`)
- Test: `internal/daemon/polecat_health_test.go`

**Interfaces:**
- `func (d *Daemon) agentBeadsDir(agentBeadID string) string` = `beads.ResolveBeadsDirForID(beads.GetTownBeadsPath(town), id)` (the resolver `ForAgentBead` uses: rig DB for rig prefixes, town for hq-).
- `listAgentBeadsJSON(rigName string, dest any) error` lists the rig's DB.
- `checkPolecatHealth`: on an agent-bead read error, logs `UNKNOWN: crash detection for <rig>/<polecat> ...` and returns; when `hook_bead` is empty, the work bead assigned to the polecat with status hooked or in_progress stands in (the slot has not been written since hq-l6mm5), so crash detection is not blind.

- [ ] **Step 1: failing tests.** Fake bd that honors `BEADS_DIR`: it returns the agent row only when `BEADS_DIR` is the rig's `.beads` (town fixture with `routes.jsonl` mapping `gt-` to `myr/mayor/rig`) and `[]`/exit 1 otherwise. Expect CRASH DETECTED (fails on main: the pinned town read misses). Second test: agent bead absent everywhere; log must contain `UNKNOWN`. Third: `hook_bead` empty, assigned work bead hooked; expect CRASH DETECTED naming that bead.
- [ ] **Step 2: run, expect FAIL.**
- [ ] **Step 3: implement**; existing fakes that ignore `BEADS_DIR` keep passing because they answer any dir.
- [ ] **Step 4: run** `go test ./internal/daemon/`.
- [ ] **Step 5: commit** `fix(daemon): read agent beads from their canonical rig DB; log not-found as UNKNOWN`.

### Task 4: witness stops restarting live polecats on the stale hook_bead slot (gt-fcxe9.7, G1-03)

**Files:**
- Modify: `internal/witness/handlers.go` (delete the "agent alive but hooked bead closed" restart branch in `detectZombieLiveSession`)
- Test: `internal/witness/handlers_test.go` (replace the tautological `TestDetectZombie_BeadClosedStillRunning`)

- [ ] **Step 1: failing test.** Real tmux socket session running `sleep`, `GT_PROCESS_NAMES=sleep`, snapshot `HookBead` pointing at a bead the fake bd reports closed, `restartSessionExecFn` records calls; expect no restart and no `ZombieBeadClosedStillRunning` result.
- [ ] **Step 2: run, expect FAIL** (restart recorded).
- [ ] **Step 3: delete the branch**; the idle reaper in the daemon already covers an idle live polecat using the work bead assignee. Keep the classification constant (receipts and mountain still map it for historical records).
- [ ] **Step 4: run** `go test ./internal/witness/`.
- [ ] **Step 5: commit** `fix(witness): never restart a live polecat on the unmaintained hook_bead slot`.

### Task 5: gates and review

- [ ] `make lint`, `go build ./...`, package tests, `make test` with wall time, all by exit code.
- [ ] `om review -base origin/main`; fix blockers and majors.
- [ ] Attribution grep empty; `git push origin crew/sloan/w2-liveness`.
