# Retiring the mayor role

Ref: gt-rwp7z.1 (epic gt-rwp7z). Companion: [escalation.md](escalation.md),
[mail-protocol.md](mail-protocol.md), [polecat-lifecycle-patrol.md](polecat-lifecycle-patrol.md).

The mayor is the last resident town-level agent. This doc inventories every
non-test, non-doc source file that names it, splits the work into landable
slices, and names the config keys, CLI verbs and behaviours that change.

Read it before touching any `mayor` reference: the same eight letters name both
the **role** to delete and the **directory** to keep, and a blind delete breaks
the town.

## The role and the path

Two distinct things share the name:

- **The role** — a resident tmux session (`hq-mayor`), the `gt mayor` command,
  the `mayor` values in `GT_ROLE`, role templates, the mail address `mayor/`,
  the daemon's `ensure-mayor` supervision and `mayor_dispatch` patrol, the
  `hq-mayor` agent bead, the doctor checks and escalation routes that name it.
  This is what retires.
- **The path** — the directory `mayor/` at the town root and per rig
  (`mayor/town.json`, `mayor/rigs.json`, `mayor/daemon.json`,
  `mayor/overseer.json`, `mayor/accounts.json`, `mayor/config.json`) and the
  clone `<rig>/mayor/rig`. The operator keeps these names; they are still the
  town marker, the rig registry, the daemon config and the rig's working clone.

Every mark below is one of:

- **DELETE** — the file exists only to implement the mayor role; delete it (or
  reduce it to nothing).
- **EDIT** — the file stays; its mayor-role lines are removed and the rest is
  untouched.
- **KEEP** — the only references are the `mayor/...` path; no change.

## Method

`git grep -il mayor` names 713 files on `main`. Removing `docs/` and other
markdown docs (`internal/templates/townroot/claude.md` is an embedded source
template and stays in), `_test.go`, `.beads/`, the `gt-model-eval` fixtures
and the sheriff evidence leaves **283 source files**: 8 DELETE, 171 EDIT,
104 KEEP. Every file was
opened or grepped before it was classified; the per-file list is the
[appendix](#appendix-full-inventory). `mayor/town.json`, `<rig>/mayor/rig` and
the `testdata/livetown` fixtures are KEEP by construction.

`docs/plans/` and `docs/research/` are historical and out of scope; `docs/`
files generally are edited only where they describe the live role, in the
slice that changes the code they describe.

No source file changes in this bead. The gate is `make gate`.

## Deletion slices

Eight slices, in order. Each is one worker and one landing. Land them in
sequence: later slices remove the `mayor` role enum and constants that earlier
slices stop using.

### Slice 1 — Retire the mayor session, command and daemon supervision

The live agent and everything that starts, stops or watches it. This is the slice that makes the role stop existing at runtime.

**Files (22):** `internal/cmd/daemon.go`, `internal/cmd/daemon_dispatch.go`, `internal/cmd/down.go`, `internal/cmd/estop.go`, `internal/cmd/handoff.go`, `internal/cmd/mayor.go`, `internal/cmd/operator_lifecycle.go`, `internal/cmd/peek.go`, `internal/cmd/start.go`, `internal/cmd/statusline.go`, `internal/cmd/up.go`, `internal/config/daemon_config.go`, `internal/daemon/daemon.go`, `internal/daemon/dog_cycle.go`, `internal/daemon/mayor_dispatch.go`, `internal/daemon/supervisor_host.go`, `internal/daemon/townhealth.go`, `internal/daemon/types.go`, `internal/daemon/upgrade_idle.go`, `internal/land/changed.go`, `internal/mayor/manager.go`, `internal/session/town.go`.

**DELETE:** internal/mayor/manager.go (whole package), internal/cmd/mayor.go, internal/cmd/daemon_dispatch.go, internal/daemon/mayor_dispatch.go, internal/session/town.go.

**CLI verbs removed:** `gt mayor start`, `gt mayor stop`, `gt mayor attach`, `gt mayor status`, `gt mayor restart`, `gt daemon dispatch-check`.

**Config keys removed:** `patrols.mayor`, `patrols.mayor_dispatch` in `mayor/daemon.json` and the `daemon` section of `settings/config.json`.

**Stale-config/stale-data handling:** An existing `settings/config.json` that still carries `"patrols": {"mayor": {...}, "mayor_dispatch": {...}}` is tolerated and ignored: the JSON decoder drops unknown keys and `isPatrolActive` returns false for a retired patrol. Ship the default config without the two keys. No hard error, so a shared or downgraded config still starts.

**Must still pass:** `make gate`; `gt up`/`gt down` start and stop the remaining services; the daemon still supervises witness/refinery/polecat seats.

### Slice 2 — Remove `mayor/` from mail, nudge and escalation routing

The address, the role shortcut and the escalation action.

**Files (27):** `internal/cmd/compact_report.go`, `internal/cmd/doctor_fix.go`, `internal/cmd/dolt.go`, `internal/cmd/escalate.go`, `internal/cmd/mail.go`, `internal/cmd/mail_directory.go`, `internal/cmd/mail_group.go`, `internal/cmd/mail_identity.go`, `internal/cmd/mail_send.go`, `internal/cmd/nudge.go`, `internal/cmd/nudge_expiry.go`, `internal/cmd/tap_guard_permission_request.go`, `internal/config/types.go`, `internal/daemon/attention.go`, `internal/daemon/dolt.go`, `internal/daemon/jsonl_git_backup.go`, `internal/daemon/maintenance_seams.go`, `internal/doctor/authorize.go`, `internal/doltserver/cleanup_guard.go`, `internal/mail/mailbox.go`, `internal/mail/resolve.go`, `internal/mail/router.go`, `internal/mail/types.go`, `internal/notify/escalate.go`, `internal/notify/notify.go`, `internal/notify/notifyfake/contract.go`, `internal/nudge/deliver/town.go`.

**DELETE:** none.

**CLI verbs removed:** none.

**Config keys removed:** every `"mail:mayor"` entry in `settings/escalation.json` and the `escalation.routes` defaults in `internal/config/types.go`.

**Stale-config/stale-data handling:** A route list that still contains `"mail:mayor"` is tolerated: the action is dropped at parse and logged once (`ignoring retired escalation action "mail:mayor"`). Dropping keeps a raised severity from failing closed on an unmigrated town; the remaining actions in the list still fire.

**Must still pass:** `make gate`; mail to every other address; the `@town` broadcast; escalation routing at each severity after the mayor entry is dropped.

### Slice 3 — Remove `mayor` from dispatch, holds and agent beads

Dispatch targets, dispatch holds, the town agent bead and the reaper exemption.

**Files (29):** `internal/beads/agent_ids.go`, `internal/beads/beads.go`, `internal/beads/beads_agent.go`, `internal/beads/fields.go`, `internal/beads/routes.go`, `internal/cmd/agent_pause.go`, `internal/cmd/hook.go`, `internal/cmd/hooked_work.go`, `internal/cmd/polecat.go`, `internal/cmd/polecat_cycle.go`, `internal/cmd/polecat_helpers.go`, `internal/cmd/polecat_spawn.go`, `internal/cmd/ready.go`, `internal/cmd/reaper.go`, `internal/cmd/sling.go`, `internal/cmd/sling_helpers.go`, `internal/cmd/sling_pool.go`, `internal/cmd/sling_target.go`, `internal/cmd/sling_validate.go`, `internal/cmd/slot.go`, `internal/cmd/spec.go`, `internal/cmd/tail_filter.go`, `internal/cmd/unsling.go`, `internal/dispatch/dispatch_hold.go`, `internal/dispatch/hold.go`, `internal/dispatch/operator.go`, `internal/reaper/reaper.go`, `internal/slot/reap.go`, `internal/specdispatch/dispatch.go`.

**DELETE:** none.

**CLI verbs removed:** `gt sling <bead> mayor`, `gt hook show mayor`, the `mayor` target in `gt unsling`.

**Config keys removed:** the `needs-mayor-review` label and the `roles.overrides.mayor` theme/role key.

**Stale-config/stale-data handling:** Beads already carrying `needs-mayor-review` are no longer held by it — the label is unknown to the dispatcher and the bead dispatches as ordinary ready work. This is the one stale value that changes behaviour rather than being ignored; name it in the landing note so an operator can re-label held beads first.

**Must still pass:** `make gate`; dispatch to every other target; `gt hook`, `gt unsling` and `gt ready` for the remaining agents; `gt reaper` still protects agent beads by the `gt:agent` label.

### Slice 4 — Remove the mayor row from the agent surface and dashboard

The rows and types that display the mayor as a live agent.

**Files (10):** `internal/cmd/agent_state.go`, `internal/cmd/agents.go`, `internal/cmd/agents_resolve.go`, `internal/cmd/dashboard_polecats.go`, `internal/cmd/status.go`, `internal/intent/intent.go`, `internal/townstatus/agents.go`, `internal/townstatus/display.go`, `internal/townstatus/model.go`, `internal/townstatus/townstatus.go`.

**DELETE:** none.

**CLI verbs removed:** none (the `gt agents`, `gt status` and dashboard rows disappear).

**Config keys removed:** none.

**Stale-config/stale-data handling:** none.

**Must still pass:** `make gate`; `gt agents`, `gt status` and the dashboard render cleanly with no global agents; the golden output in `internal/cmd/testdata` is regenerated in the same slice.

### Slice 5 — Retire the mayor doctor checks and validators

The checks that report on the mayor as an agent, not the surviving directory.

**Files (21):** `internal/cmd/doctor.go`, `internal/cmd/hooks_install.go`, `internal/cmd/hooks_reconcile.go`, `internal/cmd/hooks_sync.go`, `internal/doctor/agent_beads_check.go`, `internal/doctor/claude_settings_check.go`, `internal/doctor/env_check.go`, `internal/doctor/hook_check.go`, `internal/doctor/hooks_live_fire_check.go`, `internal/doctor/orphan_check.go`, `internal/doctor/priming_check.go`, `internal/doctor/stale_agent_beads_check.go`, `internal/doctor/stale_runtime_files_check.go`, `internal/doctor/stale_task_dispatch_check.go`, `internal/doctor/test_leak_check.go`, `internal/doctor/theme_check.go`, `internal/doctor/tmux_check.go`, `internal/doctor/town_claude_md_check.go`, `internal/doctor/unregistered_beads_check.go`, `internal/hooks/installer.go`, `internal/hooks/merge.go`.

**DELETE:** none.

**CLI verbs removed:** none.

**Config keys removed:** none.

**Stale-config/stale-data handling:** A stale `mayor/.claude/settings.json` is no longer recreated or repaired; the file is left on disk.

**Must still pass:** `make gate`; `gt doctor` on a town whose mayor session is gone reports no new failures; `gt doctor fix` no longer deletes `mayor/.claude/settings.json`.

### Slice 6 — Retire the mayor from prompts, templates, formulas and guards

Prose and templates that tell agents to mail or act as the mayor.

**Files (23):** `.githooks/pre-push`, `internal/cmd/prime_output.go`, `internal/cmd/prime_system_prompt_spawn.go`, `internal/cmd/tap_guard.go`, `internal/cmd/tap_guard_container_suite.go`, `internal/cmd/tap_guard_dangerous.go`, `internal/cmd/tap_guard_mol_patrol.go`, `internal/cmd/tap_guard_polecat_paths.go`, `internal/cmdtree/agent-bd-baseline.txt`, `internal/formula/formulas/beads-release.formula.toml`, `internal/formula/formulas/gastown-release.formula.toml`, `internal/formula/formulas/mol-dep-propagate.formula.toml`, `internal/formula/formulas/mol-gastown-boot.formula.toml`, `internal/formula/formulas/mol-orphan-scan.formula.toml`, `internal/formula/formulas/mol-polecat-work.formula.toml`, `internal/formula/formulas/mol-session-gc.formula.toml`, `internal/templates/roles/crew.md.tmpl`, `internal/templates/roles/mayor.md.tmpl`, `internal/templates/roles/polecat.md.tmpl`, `internal/templates/townroot/claude.md`, `scripts/docs-lint.sh`, `scripts/guards/context-budget-guard.sh`, `scripts/guards/context-budget-guard_test.sh`.

**DELETE:** internal/templates/roles/mayor.md.tmpl, internal/formula/formulas/mol-gastown-boot.formula.toml.

**CLI verbs removed:** none.

**Config keys removed:** none.

**Stale-config/stale-data handling:** `internal/cmdtree/agent-bd-baseline.txt` is a generated baseline whose `mayor.md.tmpl` lines must shrink in the same commit that deletes the template, or `TestAgentProseBdAllowlist` fails.

**Must still pass:** `make gate`; the release formulas still refuse a polecat session (the rule becomes crew-only); `gt prime` for every remaining role.

### Slice 7 — Remove the `mayor` role from the kernel

Last among the code slices: it removes the `RoleMayor` enum and every constant the earlier slices stopped using.

**Files (33):** `internal/cmd/done_agent_state.go`, `internal/cmd/done_close_invariant.go`, `internal/cmd/hooks_override.go`, `internal/cmd/molecule_await_event.go`, `internal/cmd/molecule_await_signal.go`, `internal/cmd/molecule_emit_event.go`, `internal/cmd/molecule_status.go`, `internal/cmd/prime.go`, `internal/cmd/rig_detect.go`, `internal/cmd/role.go`, `internal/cmd/theme.go`, `internal/config/cost_tier.go`, `internal/config/env.go`, `internal/config/loader.go`, `internal/config/roles.go`, `internal/config/roles/mayor.toml`, `internal/config/system_prompt.go`, `internal/constants/bookkeeping_beads.go`, `internal/constants/constants.go`, `internal/daemon/pressure.go`, `internal/hooks/config.go`, `internal/hookutil/roletype.go`, `internal/role/role.go`, `internal/runtime/runtime.go`, `internal/session/identity.go`, `internal/session/legacy_socket.go`, `internal/session/lifecycle.go`, `internal/session/names.go`, `internal/session/startup.go`, `internal/templates/templates.go`, `internal/tmux/theme.go`, `internal/tmux/theme_resolver.go`, `internal/tmux/tmux.go`.

**DELETE:** internal/config/roles/mayor.toml.

**CLI verbs removed:** none.

**Config keys removed:** `models.mayor`, `cost_tiers.*.roles.mayor`, `window_tint.role_defaults.mayor`, the `mayor` role override key, the `mayor-config` file type.

**Stale-config/stale-data handling:** Unknown role keys are ignored: a `settings/config.json` that still names `mayor` under `models`, a cost tier or `window_tint.role_defaults` loads without error and the key has no effect. `mayor/config.json` itself is a kept path; only its role-shaped contents lose the `mayor` entry.

**Must still pass:** `make gate`; every remaining role still parses from `GT_ROLE`, `gt role`, prime and molecule commands.

### Slice 8 — Install, town config and shared-code comments

The residue: install stops minting the town mayor, and the remaining comment-only references go.

**Files (14):** `internal/agentpause/agentpause.go`, `internal/channelevents/channelevents.go`, `internal/cmd/bd_handshake.go`, `internal/cmd/commit.go`, `internal/cmd/config.go`, `internal/cmd/install.go`, `internal/crew/manager.go`, `internal/daemon/plugin_script.go`, `internal/done/done.go`, `internal/done/done_rebase.go`, `internal/events/events.go`, `internal/git/git.go`, `internal/polecat/namepool.go`, `internal/polecat/respawn_count.go`.

**DELETE:** none.

**CLI verbs removed:** none beyond slice 1; `gt install` stops printing the `gt mayor attach` hint.

**Config keys removed:** none beyond slice 7; `GT_MAYOR`, `GT_ROLE=mayor` and `BD_ACTOR=mayor` stop being set or read.

**Stale-config/stale-data handling:** A town whose Dolt still holds an `hq-mayor` agent bead keeps it; nothing reads it, and `gt reaper` no longer exempts it by name (only by the `gt:agent` label), so it is reapable as a stale agent bead.

**Must still pass:** `make gate`; `gt install` on a fresh directory produces `mayor/town.json` and no mayor session; `gt doctor` passes on that town.

## Risks

- **Escalation routing (slice 2).** `mail:mayor` is the default destination for
  medium, high and critical escalations. Remove it without a replacement and
  escalations fall back to a bead only. The slice must decide the replacement
  (the operator, an on-call mail address, or bead-only) before the route list
  is edited.
- **Dispatch (slices 1, 3).** The `mayor_dispatch` patrol is the only thing
  that nudges a human-facing agent when ready work and free seats coexist. The
  spec dispatcher slings without it, so the town keeps moving, but nothing
  surfaces "seats are empty, work is ready" any more. Confirm the steward/plan
  patrols cover that signal.
- **Mail delivery (slice 2).** `mayor/` is a well-known address and `@town`
  resolves to the town-level agents. `@town` must still resolve (to deacon or
  to nothing) rather than becoming an unknown address that bounces.
- **`gt up` / `gt down` (slice 1).** Both list the mayor as a service and
  print it in their summary. Removing the session without the service table
  leaves a phantom line or a failed start; the golden output in
  `internal/cmd/testdata` is regenerated in the slice.
- **Doctor (slice 5).** Several checks are *named* for the role but *validate
  the surviving directory* (`mayor-exists`, `mayor-clone-exists`,
  `testutil-symlink`, `town-config`). Deleting by name rather than by
  behaviour would break the rig clone and town-config checks.
- **Dashboard (slice 4).** The global-agent section exists to show the mayor.
  With the session gone it must render empty, not error; the statusline
  branch that special-cased the mayor session goes with the session (slice 1).
- **Agent bead reaping (slices 3, 8).** The reaper exempts `%-mayor` by id.
  Removing the exemption while an `hq-mayor` bead still exists in Dolt makes
  it reapable; that is intended, but name it so an operator is not surprised
  by a vanished agent bead.
- **Generated baselines (slices 4, 6).** `internal/cmdtree/agent-bd-baseline.txt`
  and `internal/cmd/testdata/*` are generated; they fail closed when the
  source shrinks without them.

## What this bead does not do

No code, config, template or script is changed here; no directory is renamed;
the new-planner design is out of scope. This doc is the map the operator files
the eight slices from.

---

## Appendix: full inventory

Every non-test, non-doc source file that `git grep -il mayor` names, grouped by
directory. **D** = DELETE (the role); **E** = EDIT (remove the role references);
**K** = KEEP (the directory name).

### `(repository root)`

| File | Mark | Note |
|------|------|------|
| `.dockerignore` | K | Ignore rule for the mayor/ directory. |
| `Makefile` | K | mayor/rig path in a comment. |
| `docker-entrypoint.sh` | K | Checks /gt/mayor/town.json. |

### `.githooks`

| File | Mark | Note |
|------|------|------|
| `pre-push` | E | Comments name mayor as a session type. |

### `internal/agentpause`

| File | Mark | Note |
|------|------|------|
| `agentpause.go` | E | PausedBy value and role-list comments. |

### `internal/beads`

| File | Mark | Note |
|------|------|------|
| `agent_ids.go` | E | MayorBeadIDTown and the town-agent role switch. |
| `beads.go` | E | Comment names mayor among town-level agents. |
| `beads_agent.go` | E | Role switch and RoleType comment. |
| `beads_redirect.go` | K | mayor/rig/.beads redirect paths only. |
| `beads_types.go` | K | mayor/town.json town-root marker. |
| `fields.go` | E | "hq-mayor" example in a comment. |
| `routes.go` | E | Comment names Mayor among hq- issues. |
| `sling_context_scan.go` | K | Scans <rig>/mayor/rig for context. |

### `internal/channelevents`

| File | Mark | Note |
|------|------|------|
| `channelevents.go` | E | Comment "e.g. mayor" channel. |

### `internal/cmd`

| File | Mark | Note |
|------|------|------|
| `agent_pause.go` | E | mayor target address and role list. |
| `agent_state.go` | E | hq-mayor in help examples. |
| `agents.go` | E | AgentMayor type, colour, sort order, rows. |
| `agents_resolve.go` | E | Role flag help names mayor. |
| `bd_handshake.go` | E | "gt mayor start/restart" in the allowlist. |
| `commit.go` | E | Comment "mayor/" example. |
| `compact_report.go` | E | sendMayorMail and the mayor/ recipient. |
| `config.go` | E | mayor/town.json help plus a retired-nudge line. |
| `config_layout.go` | K | mayor/town.json and settings/config.json. |
| `crew_add.go` | K | mayor/rigs.json. |
| `crew_at.go` | K | MayorAccountsPath (mayor/accounts.json). |
| `crew_lifecycle.go` | K | RigMayorPath, MayorAccountsPath. |
| `daemon.go` | E | Comment example name "mayor". |
| `daemon_dispatch.go` | D | `gt daemon dispatch-check` (the mayor_dispatch patrol command). |
| `dashboard_polecats.go` | E | needs-mayor-review label. |
| `doctor.go` | E | mayor-exists / mayor-clone-exists help lines. |
| `doctor_fix.go` | E | mayor/overseer approval text. |
| `dolt.go` | E | Comments on mayor decisions and escalation. |
| `done_agent_state.go` | E | RoleMayor case. |
| `done_close_invariant.go` | E | Role-list comment. |
| `down.go` | E | Stops the mayor session. |
| `escalate.go` | E | mail:mayor route in help. |
| `estop.go` | E | MayorSessionName in the stop list. |
| `gitinit.go` | K | Gitignore patterns for mayor/rig. |
| `handoff.go` | E | mayor role shortcut and handoff path. |
| `helpers.go` | K | mayor/rigs.json. |
| `hook.go` | E | mayor target handling and examples. |
| `hooked_work.go` | E | rigName == "mayor". |
| `hooks_install.go` | E | mayor/deacon directory handling. |
| `hooks_override.go` | E | mayor override target. |
| `hooks_reconcile.go` | E | Removes the mayor's interim hook. |
| `hooks_sync.go` | E | Comment: targets[0] is the mayor target. |
| `init.go` | K | mayor/ ignore comment. |
| `install.go` | E | Creates mayor/, the hq-mayor bead, mayor settings, attach hint. |
| `log.go` | K | Stat of the mayor/ directory. |
| `mail.go` | E | Mayor inbox docs and examples. |
| `mail_directory.go` | E | mayor/ well-known address. |
| `mail_group.go` | E | Comment address list. |
| `mail_identity.go` | E | mayor identity resolution. |
| `mail_send.go` | E | Comment on "mayor" vs "mayor/". |
| `mayor.go` | D | The `gt mayor start|stop|attach|status|restart` command tree. |
| `molecule_await_event.go` | E | Comment "mayor" channel. |
| `molecule_await_signal.go` | E | Comment "mayor" channel. |
| `molecule_emit_event.go` | E | Comment "mayor" channel. |
| `molecule_status.go` | E | mayor identity and bead lookup. |
| `nudge.go` | E | mayor shortcut. |
| `nudge_expiry.go` | E | Falls back to the mayor as owner. |
| `operator_lifecycle.go` | E | mayorSeat and superviseMayor. |
| `peek.go` | E | peek of the mayor session. |
| `plugin.go` | K | mayor/rig/plugins path. |
| `polecat.go` | E | Escalate-to-mayor strings. |
| `polecat_capacity.go` | K | mayor/rigs.json. |
| `polecat_cycle.go` | E | Guard on the mayor name. |
| `polecat_helpers.go` | E | Mail-mayor recovery hint. |
| `polecat_spawn.go` | E | RoleMayor case (rest is mayor/rig paths). |
| `prime.go` | E | RoleMayor context and bead id. |
| `prime_output.go` | E | outputMayorContext. |
| `prime_system_prompt_spawn.go` | E | RoleMayor mapping. |
| `ready.go` | E | mayor/rigs.json plus a mayor-dispatch comment. |
| `reaper.go` | E | Role-list comment. |
| `report.go` | K | mayor/rig paths. |
| `rig.go` | K | mayor/rigs.json and mayor/rig. |
| `rig_config.go` | K | mayor/rigs.json. |
| `rig_detect.go` | E | RoleMayor in the skip list. |
| `rig_dock.go` | K | mayor/rig path. |
| `rig_helpers.go` | K | MayorRigsPath and mayor/rig. |
| `rig_park.go` | K | mayor/rigs.json. |
| `rig_quick_add.go` | K | mayor directory check. |
| `role.go` | E | mayor role parsing. |
| `session.go` | K | mayor/rigs.json. |
| `sling.go` | E | mayor sling target. |
| `sling_batch.go` | K | mayor/rigs.json and mayor/rig. |
| `sling_helpers.go` | E | MayorRigsPath plus mayor bead id. |
| `sling_pool.go` | E | mayor comment. |
| `sling_target.go` | E | RoleMayor trailing slash. |
| `sling_validate.go` | E | mayor has no sub-agents. |
| `slot.go` | E | mayor comment. |
| `spec.go` | E | mayor_dispatch comparison and needs-mayor-review label. |
| `start.go` | E | Kills the mayor session in order. |
| `status.go` | E | mayor emoji and global-agent row. |
| `statusline.go` | E | mayor statusline branch. |
| `tail_filter.go` | E | "Mayor patrol" filter regex. |
| `tap_guard.go` | E | GT_MAYOR env var. |
| `tap_guard_container_suite.go` | E | Role-list comment. |
| `tap_guard_dangerous.go` | E | mayor scope comments and allow paths. |
| `tap_guard_mol_patrol.go` | E | Allows the Mayor to run mol patrol. |
| `tap_guard_permission_request.go` | E | Escalates a parked prompt to the mayor. |
| `tap_guard_polecat_paths.go` | E | Comments. |
| `tap_guard_town_scan.go` | K | mayor/ scan paths. |
| `theme.go` | E | Mayor theme. |
| `unsling.go` | E | mayor target. |
| `up.go` | E | Starts the mayor session. |

### `internal/cmd/testdata`

| File | Mark | Note |
|------|------|------|
| `tail_busy_sample.txt` | K | Golden log fixture. |
| `tail_golden.txt` | K | Golden log fixture. |

### `internal/cmdtree`

| File | Mark | Note |
|------|------|------|
| `agent-bd-baseline.txt` | E | Baseline lines counting bd verbs in mayor.md.tmpl. |

### `internal/config`

| File | Mark | Note |
|------|------|------|
| `bd_metadata.go` | K | mayor/rig/.beads path. |
| `cost_tier.go` | E | mayor key in cost/tier model maps. |
| `daemon_config.go` | E | mayor and mayor_dispatch patrol config. |
| `env.go` | E | RoleMayor env setup. |
| `layout.go` | K | mayor/town.json layout. |
| `layout_migrate.go` | K | mayor/town.json and mayor/rigs.json. |
| `loader.go` | E | LoadMayorConfig and role/path markers. |
| `overseer.go` | K | mayor/overseer.json path. |
| `roles.go` | E | Role list including mayor. |
| `system_prompt.go` | E | RoleMayor case. |
| `types.go` | E | MayorConfig plus role and escalation keys. |

### `internal/config/roles`

| File | Mark | Note |
|------|------|------|
| `mayor.toml` | D | mayor role definition (session, prompt, env). |

### `internal/config/testdata/livetown/mayor`

| File | Mark | Note |
|------|------|------|
| `daemon.json` | K | testdata fixture. |

### `internal/config/testdata/livetown/settings`

| File | Mark | Note |
|------|------|------|
| `config.json` | K | testdata fixture. |
| `escalation.json` | K | testdata fixture. |

### `internal/constants`

| File | Mark | Note |
|------|------|------|
| `bookkeeping_beads.go` | E | Comment names the mayor. |
| `constants.go` | E | DirMayor/path helpers stay; RoleMayor and EmojiMayor go. |

### `internal/crew`

| File | Mark | Note |
|------|------|------|
| `manager.go` | E | mayor/rig paths plus the "mayor" remote name. |

### `internal/daemon`

| File | Mark | Note |
|------|------|------|
| `attention.go` | E | mayorAddress and BLOCKED mail. |
| `attention_tips.go` | K | mayor/rig path. |
| `daemon.go` | E | mayor dispatch ticker and ensureMayorRunning. |
| `dog_cycle.go` | E | mayor_dispatch comment. |
| `dolt.go` | E | Escalation mail to mayor/. |
| `git_hygiene.go` | K | mayor/rig paths. |
| `handler.go` | K | mayor/rigs.json. |
| `jsonl_git_backup.go` | E | escalateAlert sends to the mayor. |
| `log_rotation.go` | K | mayor/rig/.beads log path. |
| `maintenance_seams.go` | E | Comment: escalate to the mayor. |
| `mayor_dispatch.go` | D | mayor_dispatch patrol implementation. |
| `plugin_script.go` | E | Comment "until the mayor closes it". |
| `pressure.go` | E | RoleMayor in the infrastructure-agent list. |
| `rig_status.go` | K | mayor/rigs.json. |
| `supervisor_host.go` | E | mayor.Manager Stop and RoleMayor. |
| `town_config.go` | K | mayor/town.json etc. |
| `townhealth.go` | E | mayor_dispatch interval row. |
| `types.go` | E | mayor/daemon.json plus mayor patrol. |
| `upgrade_idle.go` | E | mayorDispatchRunning and mayor/rig path. |

### `internal/dispatch`

| File | Mark | Note |
|------|------|------|
| `dispatch_hold.go` | E | needs-mayor-review label and MAYOR DESIGN DECISION prose. |
| `hold.go` | E | mayor as decision authority comment. |
| `operator.go` | E | mayor/deacon address handling. |

### `internal/doctor`

| File | Mark | Note |
|------|------|------|
| `agent_beads_check.go` | E | mayor agent bead. |
| `authorize.go` | E | mayor/overseer authorization text. |
| `beads_check.go` | K | mayor/rigs.json. |
| `beads_redirect_target_check.go` | K | mayor/rig/.beads. |
| `branch_check.go` | K | mayor dir skip and mayor/rig. |
| `claude_settings_check.go` | E | mayor agent settings check. |
| `config_check.go` | K | mayor/.claude path and dir skip. |
| `config_layout_check.go` | K | mayor/town.json. |
| `crew_check.go` | K | mayor dir skip. |
| `dolt_server_patrol_check.go` | K | mayor/daemon.json. |
| `editorial_required_check.go` | K | mayor/rig/.om.json. |
| `env_check.go` | E | RoleMayor in the role map. |
| `events_journal_check.go` | K | mayor/rigs.json. |
| `events_log_check.go` | K | mayor/daemon.json. |
| `foreign_remote_check.go` | K | mayor/rig comment. |
| `hook_check.go` | E | agent == "mayor" directory lookup. |
| `hooks_live_fire_check.go` | E | GT_MAYOR env var. |
| `hooks_path_all_rigs_check.go` | K | mayor/rig clone. |
| `idle_timeout_check.go` | K | gastown/mayor/rig path. |
| `lifecycle_defaults_check.go` | K | mayor/daemon.json. |
| `migration_check.go` | K | mayor/rig/.beads. |
| `null_assignee_check.go` | K | mayor/rigs.json. |
| `orphan_check.go` | E | mayor session validity. |
| `overlay_health_check.go` | K | mayor/rigs.json. |
| `patrol_check.go` | K | mayor/rig paths. |
| `priming_check.go` | E | mayor priming checks. |
| `rig_bd_binary_check.go` | K | mayor scan subdir. |
| `rig_beads_check.go` | K | mayor/rig route. |
| `rig_check.go` | K | mayor/rig clone checks. |
| `rig_config_sync_check.go` | K | mayor/rigs.json and mayor/rig. |
| `rig_name_check.go` | K | mayor/rigs.json. |
| `rig_routes_jsonl_check.go` | K | mayor/town.json and mayor/rigs.json. |
| `rigs_json_check.go` | K | mayor/rigs.json. |
| `routes_check.go` | K | mayor/rig default path. |
| `sparse_checkout_check.go` | K | mayor dir skip. |
| `stale_agent_beads_check.go` | E | Skips town-level agents comment. |
| `stale_beads_redirect_check.go` | K | mayor/rig/.beads. |
| `stale_dolt_port_check.go` | K | mayor/rig/.beads. |
| `stale_runtime_files_check.go` | E | hq-mayor.pid example. |
| `stale_sql_server_info_check.go` | K | mayor/rigs.json. |
| `stale_task_dispatch_check.go` | E | Comment on stale Mayor settings. |
| `stalled_polecat_check.go` | K | mayor dir skip. |
| `test_leak_check.go` | E | Role-name set. |
| `testutil_symlink_check.go` | K | mayor/rig/internal/testutil. |
| `theme_check.go` | E | [Mayor] legacy-format comment. |
| `tmux_check.go` | E | mayor session exclusion. |
| `town_claude_md_check.go` | E | Comment "e.g. Mayor". |
| `town_config_check.go` | K | mayor/town.json etc. |
| `unregistered_beads_check.go` | E | "mayor" skip map. |
| `workspace_check.go` | K | mayor/ directory and town.json/rigs.json checks. |
| `worktree_gitdir_check.go` | K | mayor marker dir. |

### `internal/doltserver`

| File | Mark | Note |
|------|------|------|
| `beads_dir_for_database.go` | K | mayor/rigs.json. |
| `cleanup_guard.go` | E | mayor/overseer authorization text. |
| `doltserver.go` | K | mayor/town.json and mayor/rig/.beads. |

### `internal/done`

| File | Mark | Note |
|------|------|------|
| `done.go` | E | mayor actor handling plus paths. |
| `done_rebase.go` | E | Comment citing the mayor. |

### `internal/events`

| File | Mark | Note |
|------|------|------|
| `events.go` | E | hq-mayor example. |

### `internal/formula/formulas`

| File | Mark | Note |
|------|------|------|
| `beads-release.formula.toml` | E | "crew or mayor" session rule. |
| `gastown-release.formula.toml` | E | "crew or mayor" session rule and mayor rig check. |
| `mol-dep-propagate.formula.toml` | E | Suggests mailing the mayor. |
| `mol-gastown-boot.formula.toml` | D | The mayor's boot molecule. |
| `mol-orphan-scan.formula.toml` | E | Escalations mailed to the mayor. |
| `mol-polecat-work.formula.toml` | E | Scope questions mailed to the mayor. |
| `mol-session-gc.formula.toml` | E | Authorization bead from the mayor. |

### `internal/git`

| File | Mark | Note |
|------|------|------|
| `git.go` | E | mayor/town.json markers plus the "Mayor-managed fork" error. |

### `internal/hooks`

| File | Mark | Note |
|------|------|------|
| `config.go` | E | mayor override target and settings path. |
| `installer.go` | E | Role comment. |
| `merge.go` | E | Role override comment. |

### `internal/hookutil`

| File | Mark | Note |
|------|------|------|
| `roletype.go` | E | Interactive-roles comment. |

### `internal/intent`

| File | Mark | Note |
|------|------|------|
| `intent.go` | E | Seat comment. |

### `internal/land`

| File | Mark | Note |
|------|------|------|
| `changed.go` | E | sessionStartFiles entry for internal/mayor/manager.go. |

### `internal/mail`

| File | Mark | Note |
|------|------|------|
| `mailbox.go` | E | mayor address variants. |
| `resolve.go` | E | mayor address resolution. |
| `router.go` | E | mayor routing and @town. |
| `types.go` | E | mayor address normalisation. |

### `internal/mayor`

| File | Mark | Note |
|------|------|------|
| `manager.go` | D | mayor session manager (whole package). |

### `internal/notify`

| File | Mark | Note |
|------|------|------|
| `escalate.go` | E | Parses mail:mayor. |
| `notify.go` | E | Doc-comment examples. |

### `internal/notify/notifyfake`

| File | Mark | Note |
|------|------|------|
| `contract.go` | E | Test fake uses mayor addresses. |

### `internal/nudge/deliver`

| File | Mark | Note |
|------|------|------|
| `town.go` | E | mayor nudge target. |

### `internal/patrolscan`

| File | Mark | Note |
|------|------|------|
| `roguebd.go` | K | Scans the mayor tree. |

### `internal/plugin`

| File | Mark | Note |
|------|------|------|
| `sync.go` | K | mayor/rig/plugins. |

### `internal/polecat`

| File | Mark | Note |
|------|------|------|
| `gitseam.go` | K | mayor/rig path. |
| `manager.go` | K | mayor/rig fallback. |
| `namepool.go` | E | "mayor" reserved name. |
| `respawn_count.go` | E | Escalate-to-mayor comment. |
| `surviving_branch.go` | K | mayor/rig fallback. |
| `surviving_work.go` | K | mayor/rig error string. |

### `internal/reaper`

| File | Mark | Note |
|------|------|------|
| `reaper.go` | E | gt:agent comment and %-mayor exclusion. |

### `internal/rig`

| File | Mark | Note |
|------|------|------|
| `manager.go` | K | mayor/rig clone. |
| `opstate.go` | K | mayor/rigs.json. |
| `types.go` | K | mayor/rig paths and HasMayor. |

### `internal/role`

| File | Mark | Note |
|------|------|------|
| `role.go` | E | mayor role enum member. |

### `internal/runtime`

| File | Mark | Note |
|------|------|------|
| `runtime.go` | E | Comment on the mayor work dir. |

### `internal/schedulerrun`

| File | Mark | Note |
|------|------|------|
| `contexts.go` | K | mayor/rig/.beads. |
| `validate.go` | K | MayorRigsPath. |

### `internal/session`

| File | Mark | Note |
|------|------|------|
| `identity.go` | E | RoleMayor parsing. |
| `legacy_socket.go` | E | MayorSessionName cases. |
| `lifecycle.go` | E | Role comments. |
| `names.go` | E | MayorSessionName. |
| `registry.go` | K | mayor/rigs.json and mayor/town.json. |
| `startup.go` | E | Comment example. |
| `town.go` | D | Town-level session list (only the Mayor). |
| `window_tint.go` | K | mayor/config.json. |

### `internal/slot`

| File | Mark | Note |
|------|------|------|
| `reap.go` | E | mayor comment. |

### `internal/specdispatch`

| File | Mark | Note |
|------|------|------|
| `dispatch.go` | E | needs-mayor-review label and mayor patrol comment. |

### `internal/templates`

| File | Mark | Note |
|------|------|------|
| `templates.go` | E | Role and MayorSession fields. |

### `internal/templates/roles`

| File | Mark | Note |
|------|------|------|
| `crew.md.tmpl` | E | Deputy Mayor and mail mayor/. |
| `mayor.md.tmpl` | D | mayor role prompt. |
| `polecat.md.tmpl` | E | Mail mayor/. |

### `internal/templates/townroot`

| File | Mark | Note |
|------|------|------|
| `claude.md` | E | Line naming the Mayor as escalation recipient. |

### `internal/testutil`

| File | Mark | Note |
|------|------|------|
| `hermetic.go` | K | mayor/town.json sandbox. |
| `townenv.go` | K | mayor/rigs.json. |

### `internal/tmux`

| File | Mark | Note |
|------|------|------|
| `theme.go` | E | MayorTheme. |
| `theme_resolver.go` | E | RoleMayor and mayor/config.json. |
| `tmux.go` | E | Role comments and emoji map. |

### `internal/townconfig`

| File | Mark | Note |
|------|------|------|
| `database.go` | K | mayor/rig/.beads. |
| `parked.go` | K | mayor/rigs.json. |
| `townconfig.go` | K | mayor/town.json etc. |

### `internal/townstatus`

| File | Mark | Note |
|------|------|------|
| `agents.go` | E | mayor discovery. |
| `display.go` | E | RoleMayor. |
| `model.go` | E | Comment. |
| `townstatus.go` | E | Town-agent-beads comment. |

### `internal/version`

| File | Mark | Note |
|------|------|------|
| `stale.go` | K | gastown/mayor/rig. |

### `internal/workspace`

| File | Mark | Note |
|------|------|------|
| `find.go` | K | mayor/town.json markers. |

### `scripts`

| File | Mark | Note |
|------|------|------|
| `docs-lint.sh` | E | Comment naming a crew/mayor session. |
| `install-gt.sh` | K | mayor/rig and mayor/town.json. |
| `install-gt_test.sh` | K | mayor fixtures. |
| `run-boot-scraper-vm.sh` | K | mayor/rig path. |
| `uninstall-gt.sh` | K | mayor/town.json. |
| `uninstall-gt_test.sh` | K | mayor fixture. |

### `scripts/guards`

| File | Mark | Note |
|------|------|------|
| `context-budget-guard.sh` | E | HARD_GATE_ROLES default mayor. |
| `context-budget-guard_test.sh` | E | GT_ROLE=mayor cases. |

### `scripts/lib`

| File | Mark | Note |
|------|------|------|
| `install-gt-lib.sh` | K | mayor/town.json finder. |
