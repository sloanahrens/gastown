# gt doctor check triage: cadence / occasional / delete

> Measured 2026-09-23/24 against `internal/doctor/*.go` on `main`, not estimated. Source: [[gt-0bp42]].

gt doctor registers named checks (`CheckName` literals in `internal/doctor/*.go`), but the town runs the full sweep only occasionally (session-gc, town-shutdown, boot prose) and the deacon patrol explicitly refuses to run it inline because a full `gt doctor -v` sweep takes 60+ seconds and blocks the patrol loop (`mol-deacon-patrol.formula.toml`). That leaves most checks with no recurring consumer: a hard-failing check is invisible until a human happens to run `gt doctor` by hand — this is exactly how `deacon-self-probe` sat broken unnoticed on 2026-09-23.

This table classifies every check so a fast recurring subset can be wired into a patrol (follow-up bead) without re-litigating which checks belong in it.

## Counts

- **37 cadence** — cheap (measured), drift-prone, worth a fast recurring subset (2 already scheduled today + 35 new candidates for the follow-up)
- **83 occasional** — validates state that only changes at install/upgrade/boot/shutdown; a scheduled sweep would just re-check unchanged config
- **1 delete** — dead code, unreachable from any `gt doctor` invocation
- **121 total**, vs. 124 measured by the overseer on 2026-09-23 in the parent bead. The gap (3) reflects unrelated merges to `internal/doctor/` between that measurement and this one, not a methodology difference (same extraction: every `CheckName:` literal, deduplicated, cross-checked against every `d.Register`/`d.RegisterAll` call site in `internal/cmd/doctor.go` and `internal/cmd/upgrade.go`, `WorkspaceChecks()`, and `RigChecks()`).

Already-scheduled today, confirmed by grepping the town formulas (unaffected by this triage): `slot-debris` (the daemon's own `slot.Reap()` heartbeat path, `mol-dog-doctor.formula.toml` documents the equivalence) and `tmux-test-socket` (`gt doctor --check tmux-test-socket --fix`, `mol-deacon-patrol.formula.toml:411`). Both are excluded from the "35 additional cadence candidates" below since they need no new wiring.

## The fast subset: measured, not assumed

35 checks (beyond the 2 already scheduled) are cheap and drift-prone enough to justify a fast recurring subset. Combined via `./gt doctor --check A --check B ...` (35 flags, one process), measured runtime across two separate runs of the final set: **12.7s, 13.3s** (`user` time a flat ~3.4-3.5s both times; the rest is `sys` time in process/tmux/Dolt-query enumeration). Comfortably clear of the 60s the deacon patrol formula already treats as disqualifying for inline use. (An earlier, overlapping 34-check cut of this set measured 12.3-21.6s across four runs on a more heavily loaded town; the range narrowed once system load dropped, which is the expected shape for `sys`-time-dominated checks, not a sign the set itself is unstable.)

Two checks were tested and **rejected** from the fast subset on measured cost alone, despite being exactly the drift class this bead is about:

| Check | Standalone measured cost | Why it's too slow for the fast loop |
|---|---|---|
| `stalled-polecats` | 22.7s | Scans every live session town-wide; cost scales with polecat count |
| `identity-collision` | 9.4s | Scans all agent lock files town-wide |

Recommendation for the follow-up: wire the 35-check fast subset into a patrol (candidate: `mol-deacon-patrol`, alongside the existing `tmux-test-socket` call) on a short interval; give `stalled-polecats` and `identity-collision` their own slower cadence (e.g. hourly, or the existing `mol-session-gc` path) rather than folding them into the fast loop.

## Dead code found

`rigs-json` (`internal/doctor/rigs_json_check.go`, `NewRigsJSONCheck`) is fully implemented, including a working `Fix()`, and guards a real silent-failure class (a missing `rigs.json` breaks session-name parsing, crew cycling, and nudge routing — the check's own doc comment says so). It is never registered: not in `internal/cmd/doctor.go`, not in `internal/cmd/upgrade.go`, not in `WorkspaceChecks()`, not in `RigChecks()`. No `gt doctor` invocation can ever run it. This is the one dead-code candidate found by cross-referencing every check's constructor against every call site in the repo (methodology: grep every `New*Check()` constructor name against every non-test `.go` file, excluding the constructor's own definition line). Recommendation for the follow-up: register it (it duplicates no existing check — `rigs-registry-exists`/`rigs-registry-valid` in `WorkspaceChecks()` check only the canonical path, not `rigs-json`'s fallback-copy resilience), not delete the file.

## Methodology

1. Extract every `CheckName: "..."` literal from `internal/doctor/*.go` (excluding `_test.go`), paired with the `CheckCategory` and `CheckDescription` in the same struct literal.
2. Map each check's constructor (`New*Check` function) to every call site in the repo to determine whether it is reachable from `gt doctor` (directly, via `WorkspaceChecks()`/`RigChecks()`, or only from `gt upgrade`) or unreachable (dead).
3. Grep every town-level formula (`~/gt/.beads/formulas/*.toml`) for `gt doctor` invocations to find checks with an existing recurring caller.
4. Classify the remainder using category as a prior (`Cleanup`/`Infrastructure`(liveness)/`Patrol`/`Hooks`(runtime-state) checks default cadence; `Core`/`Config`/`Rig`/`Hooks`(install-state)/`Infrastructure`(binary-version) checks default occasional — they validate state that only changes at install, upgrade, or a deliberate operator action), with individual overrides where the description or a measurement said otherwise.
5. Build the `gt` binary from this branch and measure real runtimes with `./gt doctor --check <name>` (standalone) and the batched fast-subset command (combined), rather than estimating from the description text.

## Full classification

| Check | Category | Tier | Reason |
|---|---|---|---|
| `clone-divergence` | Cleanup | cadence | Detects emergency divergence between git clones; cheap git plumbing, and divergence compounds the longer it's unnoticed. |
| `crash-reports` | Cleanup | cadence | Cheap directory scan for recent crash reports; a crash is actionable within minutes, not just at gc/shutdown. |
| `crew-state` | Cleanup | cadence | Cheap state.json read per crew worker; crew state drifts as often as crew members act, worth catching fast. |
| `crew-worktrees` | Cleanup | cadence | Cheap directory scan for stale cross-rig worktrees; same drift profile as orphan-sessions/orphan-processes. |
| `dolt-orphan-servers` | Cleanup | cadence | Cheap process scan for abandoned test dolt sql-server instances that otherwise leak until a human notices. |
| `dolt-orphaned-databases` | Cleanup | cadence | Cheap `SHOW DATABASES` scan; orphan DBs are the same class the doctor-dog already patrols for slot-debris. |
| `jsonl-bloat` | Cleanup | cadence | Cheap file-size/staleness comparison; bloat is progressive and best caught early. |
| `misclassified-wisps` | Cleanup | cadence | Cheap Dolt query; ephemeral beads misfiled into the issues table pollute reporting until caught. |
| `null-assignee-steps` | Cleanup | cadence | Cheap Dolt query; a NULL-assignee in_progress bead is invisible to bd and blocks indefinitely -- exactly the deacon-self-probe failure class. |
| `orphan-processes` | Cleanup | cadence | Detects runtime processes outside tmux; cheap process scan, same drift profile as orphan-sessions. |
| `orphan-sessions` | Cleanup | cadence | Detects orphaned tmux sessions; cheap process/session scan, state drifts within minutes. |
| `persistent-role-branches` | Cleanup | cadence | Detects witness/refinery off main; cheap git status, and a role stuck off-main breaks its automation immediately. |
| `session-name-format` | Cleanup | cadence | Cheap tmux session-name scan for outdated naming. |
| `slot-debris` | Cleanup | cadence | ALREADY SCHEDULED: the daemon's own `slot.Reap()` heartbeat path runs this reap before `mol-dog-doctor` is even poured; `mol-dog-doctor.formula.toml:151` documents `gt doctor --check slot-debris --fix` as running the same reap. Not one of the 35 additional candidates below -- needs no new wiring. |
| `stale-beads-redirect` | Cleanup | cadence | Cheap file scan for stale .beads redirect files; a stale redirect silently misroutes beads until caught. |
| `stale-runtime-files` | Cleanup | cadence | Cheap scan for stale PID files/wisp configs after a rig is removed; cheap and left-behind state accumulates until swept. |
| `unregistered-beads-dirs` | Cleanup | cadence | Cheap directory scan; a stray .beads dir is a routing hazard best caught quickly. |
| `wisp-gc` | Cleanup | cadence | Cheap Dolt query for abandoned wisps (>1h); already time-windowed for a patrol cadence. |
| `zombie-sessions` | Cleanup | cadence | Detects tmux sessions with dead Claude processes; cheap, and a zombie blocks its polecat until noticed. |
| `hook-attachment-valid` | Hooks | cadence | Cheap Dolt lookup; a hook pointing at a closed molecule strands the agent immediately. |
| `hook-singleton` | Hooks | cadence | Cheap Dolt lookup; more than one handoff bead per agent is an active routing hazard. |
| `orphaned-attachments` | Hooks | cadence | Cheap Dolt lookup; a handoff bead for a nonexistent agent is dangling state worth catching promptly. |
| `stale-task-dispatch` | Hooks | cadence | Cheap settings.json grep for a stale guard; the guard is safety-critical (blocks dangerous commands) so drift belongs in a fast loop, not just boot. |
| `daemon` | Infrastructure | cadence | Is the daemon running -- foundational liveness, cheap process check. |
| `daemon-liveness` | Infrastructure | cadence | Verifies heartbeat is fresh AND advancing; cheap file-mtime check, catches a wedged (not just dead) daemon. |
| `disk-space` | Infrastructure | cadence | Cheap statvfs call; disk exhaustion is a fast-moving failure worth catching before it cascades. |
| `dolt-server-patrol` | Infrastructure | cadence | Detects Dolt server down while the daemon's own dolt_server patrol is disabled -- a monitor-the-monitor check, cheap TCP probe. |
| `dolt-server-reachable` | Infrastructure | cadence | MEASURED at 0.29s standalone -- a 2s-timeout TCP dial per unique configured Dolt server address, detecting the split-brain risk where metadata.json says server mode but the server is unreachable (bd would then silently create isolated local databases). Cheap and exactly the drift class this bead is about; the generic Infrastructure/binary-version default does not apply here since this check probes connectivity, not an installed version. |
| `linked-panes` | Infrastructure | cadence | Cheap tmux introspection; shared panes cause crosstalk between agents as soon as it happens. |
| `socket-split-brain` | Infrastructure | cadence | Cheap tmux server enumeration; a wrong-socket session causes silent nudge failures, worth catching fast. |
| `tmux-global-env` | Infrastructure | cadence | Cheap env var read; wrong GT_TOWN_ROOT breaks every nudge silently. |
| `tmux-test-socket` | Infrastructure | cadence | ALREADY SCHEDULED: `gt doctor --check tmux-test-socket --fix` runs from `mol-deacon-patrol.formula.toml:411` on the deacon patrol cadence. Not one of the 35 additional candidates below -- needs no new wiring. |
| `patrol-hooks-wired` | Patrol | cadence | Cheap config check that hooks actually trigger patrols; if this silently breaks, every other cadence check in this table stops firing too. |
| `patrol-molecules-exist` | Patrol | cadence | Cheap file-existence check for patrol formulas; a missing formula silently disables a whole patrol. |
| `patrol-not-stuck` | Patrol | cadence | Cheap Dolt query for wisps in_progress >1h; this IS the general form of the deacon-self-probe incident that motivated this bead. |
| `patrol-plugin-drift` | Patrol | cadence | Cheap file comparison between runtime and source plugins; drift accumulates until noticed. |
| `patrol-plugins-accessible` | Patrol | cadence | Cheap directory-readability check; an inaccessible plugin dir silently drops functionality. |
| `priming` |  | occasional | Verifies priming subsystem wiring (hooks, CLAUDE.md size, prime.md presence) across all agents; this is install/onboarding-time configuration, not a value that drifts between deploys. Note: this check's own BaseCheck leaves CheckCategory unset in source, so it renders under "Other" in `gt doctor` output -- a minor doc-vs-code gap, not addressed by this MR. |
| `stalled-polecats` | Cleanup | occasional | MEASURED at 22.7s standalone (`gt doctor --check stalled-polecats`) -- scans every live session town-wide; too expensive for a tight patrol budget despite being exactly the drift class this bead cares about. Candidate for its own slower cadence (e.g. hourly), not the fast loop. |
| `beads-custom-statuses` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `beads-custom-types` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `beads-exposure` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `claude-settings` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `commands-provisioned` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `database-prefix` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `deprecated-merge-queue-keys` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `dolt-metadata` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `env-vars` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `formulas` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `land-worktree-gitignore` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `legacy-gastown` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `lifecycle-defaults` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `lifecycle-hygiene` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `overlay-health` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `prefix-conflict` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `prefix-mismatch` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `rig-config-sync` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `rig-name-mismatch` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `rig-routes-jsonl` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `rig-settings` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `role-config-valid` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `routes-config` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `routing-mode` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `runtime-gitignore` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `session-hooks` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `stale-dolt-port` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `stale-sql-server-info` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `themes` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `town-beads-config` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `town-claude-md` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `unused-directives` | Config | occasional | Configuration/installation-drift check; the underlying file only changes when a human or `gt upgrade` edits it, so boot/upgrade/session-gc coverage is sufficient. |
| `foreign-remotes` | Core | occasional | Core workspace bootstrap/identity check; validates state that only changes at install, upgrade, or a deliberate operator action. |
| `global-state` | Core | occasional | Core workspace bootstrap/identity check; validates state that only changes at install, upgrade, or a deliberate operator action. |
| `mayor-exists` | Core | occasional | WorkspaceChecks() bundle item: validates town-root bootstrap files (town.json/rigs.json/mayor dir) that only change at install/upgrade time. |
| `rigs-registry-exists` | Core | occasional | WorkspaceChecks() bundle item: validates town-root bootstrap files (town.json/rigs.json/mayor dir) that only change at install/upgrade time. |
| `rigs-registry-valid` | Core | occasional | WorkspaceChecks() bundle item: validates town-root bootstrap files (town.json/rigs.json/mayor dir) that only change at install/upgrade time. |
| `town-config-exists` | Core | occasional | WorkspaceChecks() bundle item: validates town-root bootstrap files (town.json/rigs.json/mayor dir) that only change at install/upgrade time. |
| `town-config-valid` | Core | occasional | WorkspaceChecks() bundle item: validates town-root bootstrap files (town.json/rigs.json/mayor dir) that only change at install/upgrade time. |
| `town-git` | Core | occasional | Core workspace bootstrap/identity check; validates state that only changes at install, upgrade, or a deliberate operator action. |
| `town-root-branch` | Core | occasional | Core workspace bootstrap/identity check; validates state that only changes at install, upgrade, or a deliberate operator action. |
| `branch-protection` | Hooks | occasional | Hooks installation/config check; settings.json hook wiring changes only at install/upgrade, not continuously. |
| `hooks-base-missing` | Hooks | occasional | Hooks installation/config check; settings.json hook wiring changes only at install/upgrade, not continuously. |
| `hooks-live-fire` | Hooks | occasional | Deliberately fires a real PreToolUse guard as a live-fire pair test; too invasive/slow for a frequent loop, belongs at boot/upgrade. |
| `hooks-sync` | Hooks | occasional | Hooks installation/config check; settings.json hook wiring changes only at install/upgrade, not continuously. |
| `beads-binary` | Infrastructure | occasional | Infrastructure install/version check (binary present & version, not liveness); changes only when the tool is upgraded. |
| `boot-health` | Infrastructure | occasional | Vet-mode check on the Boot watchdog; by name and purpose this is boot-time, not a recurring patrol. |
| `claude-binary` | Infrastructure | occasional | Infrastructure install/version check (binary present & version, not liveness); changes only when the tool is upgraded. |
| `container-capacity` | Infrastructure | occasional | Diagnostic/informational (prints the Docker VM's CPU/memory bound); not a pass/fail alarm, so it belongs in setup/diagnosis, not a patrol loop. |
| `dolt-binary` | Infrastructure | occasional | Infrastructure install/version check (binary present & version, not liveness); changes only when the tool is upgraded. |
| `groq-compound-json` | Infrastructure | occasional | Makes a live network probe to an external agent for JSON-compliance; too slow/flaky for a fast cadence, occasional/diagnostic use. |
| `identity-collision` | Infrastructure | occasional | MEASURED at 9.4s standalone -- scans all agent lock files town-wide; too expensive for the fast loop. Same slower-cadence candidate as stalled-polecats. |
| `stale-binary` | Infrastructure | occasional | Infrastructure install/version check (binary present & version, not liveness); changes only when the tool is upgraded. |
| `deacon-self-probe` | Patrol | occasional | Patrol-adjacent but not itself a live drift signal; default to occasional absent a specific cadence justification. |
| `agent-beads-exist` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `agent-beads-shadow` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `bare-repo-exists` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `bare-repo-refspec` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `beads-config-valid` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `beads-redirect` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `beads-redirect-target` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `default-branch-all-rigs` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `default-branch-exists` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `editorial-coverage` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `editorial-required` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `git-exclude-configured` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `harness-drift` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `hooks-path-all-rigs` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `hooks-path-configured` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `idle-timeout-config` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `mayor-clone-exists` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `polecat-clones-valid` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `refinery-exists` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `rig-beads-exist` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `rig-is-git-repo` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `sparse-checkout` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `stale-agent-beads` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `sync-remote-owner` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `testutil-symlink` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `witness-exists` | Rig | occasional | RigChecks() bundle item, runs once per registered rig at boot/upgrade (16-check bundle multiplied by rig count -- too expensive to run whole on every patrol tick); re-evaluate per-rig only when a rig is added/changed. |
| `worktree-gitdir-valid` | Rig | occasional | Per-rig structural/config check; validates state that changes only when a rig is added or reconfigured. |
| `rigs-json` | Config | delete | Fully implemented (incl. Fix()) but never registered in doctor.go, upgrade.go, WorkspaceChecks(), or RigChecks() -- unreachable by any `gt doctor` invocation. Guards a real silent-failure class (missing rigs.json breaks session parsing/nudge routing) so the fix is to wire it in, not delete the file; listed under delete because as shipped today it is dead code that must be actively re-registered before it can be cadence or occasional. |

## Follow-ups (out of scope for this MR)

- Wire the 35-check fast subset into a patrol (candidate: `mol-deacon-patrol`) on a short interval; give `stalled-polecats`/`identity-collision` a separate slower cadence.
- Register `rigs-json` (do not delete `internal/doctor/rigs_json_check.go`).
- Add a test that exercises the alarm path for at least one check in the fast subset (a failing check reaches a human), per the parent bead's acceptance criteria.
- Report a live count of checks with no recurring consumer via tooling (e.g. `gt doctor --list-orphaned` or similar), so this classification cannot silently regrow stale.
