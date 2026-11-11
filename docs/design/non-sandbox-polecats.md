# Non-sandbox polecat inventory

Read when deciding whether a deletion is safe for a polecat path: this is the
code that can still run a polecat on the host, with each path's live callers and
a verdict. The decision it serves: polecats run in the k8s sandbox (the `sbx-*`
seats); the host seats `claude-haiku`, `claude-sonnet`, `deepseek-flash` and
`deepseek-flash-sandbox` were removed from `polecat_pool.seats` on 2026-11-06
(gt-slpty4).

## Method

The code read is `origin/main` at `65474d9e` (2026-11-06). Each row's callers
come from a grep of the non-test tree, given in **Evidence**. No live town was
read; the live facts below are the overseer's comment on gt-slpty4, restated.
Line numbers are left out of the paths table because they move; the symbol is
the reference. The docs table has no symbol to name, so it keeps the lines the
grep printed at that commit.

Two senses of a name carry the whole inventory:

- **Seat name**: an entry of `polecat_pool.seats` and the agent it names. The
  retired four are seat names.
- **Agent preset**: an entry of `agents` that a role resolves by name. The same
  strings are legitimate presets for host roles (`crew`, `om-review`), which is
  why the cost-tier presets stay.

A polecat *seat* named `claude-sonnet` and a *sling* that names `claude-sonnet`
are different debt: the first is a host seat, the second is a host spawn.

## Live-town facts (overseer, 2026-11-06)

- `gt pool show` lists `sbx-flash` (cap 5) and `sbx-haiku` (cap 0). The four
  retired seats are gone from `polecat_pool.seats`.
- `settings/config.json` still names the retired seats in about 13 lines outside
  the pool: `claude-haiku` (one line), `deepseek-flash-sandbox` (one),
  `deepseek-flash` (several), and the model and role tables. The overseer owns
  that cleanup after this inventory says which keys the code reads.
- Open beads labelled `agent:claude-haiku`, `agent:claude-sonnet`,
  `agent:deepseek-flash`, `agent:deepseek-flash-sandbox` or
  `agent:deepseek-flash-sandbox-k8s`: 0 each, across all nine rigs.

Which of those keys the code still reads: `agents.<name>` is read by
`lookupAgentConfigIfExists` and by `ResolveAgentConfigWithOverride`, so a
retired name still resolves while its settings entry exists. `role_agents` is
read for crew and the other host roles; `role_agents.polecat` is refused at load
(`internal/config/retired_keys.go`). The pool is the only source of a polecat's
seat.

## Paths

| # | Path (file:symbol) | What it does | Live callers (non-test) | Reached by a retired name today | Verdict |
|---|---|---|---|---|---|
| P1 | `internal/config/seats.go` — `Seats`, `SeatConfig.Backend` | Validates `polecat_pool.seats`; `backend: ""` or `tmux` is the host tmux backend, `openshell` requires a sandbox profile | `config.Seats` in `internal/townconfig/townconfig.go`, `internal/polecat/sandbox_seat_probe.go`, `internal/polecat/session_manager.go`, `internal/daemon/daemon.go`, `internal/daemon/crash_stale_sandbox_hook.go`, `internal/daemon/scheduled_slings_spec.go`, `internal/doctor/openshell_gateway_check.go`, `internal/cmd/session_backend.go`, `internal/cmd/spec.go`, `internal/cmd/pool.go`, `internal/cmd/pool_migrate.go`, `internal/cmd/dashboard_polecats.go`, `internal/cmd/dashboard_models.go` | No seat today. `gt pool set claude-haiku=2` passes while the settings declare that agent, and writes a seat with no backend (host) | **Delete now (B1)** |
| P2 | `internal/cmd/session_backend.go` — `sessionBackendForAgent`, `sessionBackendForAgentWith` | Picks the backend from the agent's seat; an agent with no seat gets host tmux | `internal/cmd/polecat_spawn.go` (spawn), `internal/cmd/pool_seats.go` (`townSeatBackend`, used by `internal/cmd/session.go` and `internal/cmd/up.go`) | Yes: `--agent claude-haiku` still resolves from the settings and, with no seat, falls through to tmux; a hand-edited seat naming a retired agent is accepted by `Seats(ts, nil)` and runs on tmux when it names no backend | **Delete now (B2)** |
| P3 | `internal/cmd/sling_pool.go` — `choosePoolAgent`, `poolOwnsAgent` | A requested agent with no seat returns no opinion and the request stands; the pool only caps agents it owns | `routeWithNotice` (the sling admission path), which also serves the spec dispatcher's own route | Yes, when a sling or a scheduled entry names a retired agent. The help text at `internal/cmd/sling.go` says a seat is honored or the sling is refused; a name that is no seat takes a third route, the host, which the help does not name | **Delete now (B2)** |
| P4 | `internal/cmd/pool_seats.go` — `restartPolecatOnSlungAgent`, `townSeatBackend` | Restarts a crashed polecat on the agent its intent record names, resolving that agent's backend | `internal/cmd/session.go` (`gt session start`/`restart`), `internal/cmd/up.go` | Yes for an intent record written before the cutover: the record outlives the seat, and a name with no seat restarts on host tmux | **Delete now (B2)** |
| P5 | `internal/polecat/session_manager.go` — `Start`, `SessionStartOptions.Backend` | A nil backend is the host tmux session | `internal/cmd/polecat_spawn.go` (passes one), `internal/cmd/pool_seats.go` (passes one when the town is known) | Only a start with no town root (no settings to name a backend) | **Delete now (B3)** |
| P6 | `internal/config/types.go` — `SandboxProfile.WithDefaults`, `Validate`, `SandboxDriverDocker`; `internal/session/backend_openshell.go` — `usesDockerVolumes`, `ensureCacheVolume`, `cacheDriverConfig` | An unset sandbox driver defaults to `docker`; a docker seat with a named cache volume mounts a per-rig Docker cache volume | `WithDefaults` in `internal/cmd/session_backend.go`, `internal/polecat/sandbox_seat_probe.go`, `internal/polecat/session_manager.go`, `internal/daemon/crash_stale_sandbox_hook.go`, `internal/doctor/openshell_gateway_check.go`, `internal/cmd/sandbox_endpoint.go`, `cmd/polecatcaches/main.go`; the volume path in `internal/session/backend_openshell.go` | The Docker-driver seat the k8s page calls the control is no longer in the pool (it is the two `sbx-*` seats); any profile that omits `driver` still takes this path, and X confirms no live `sbx-*` profile does | **Delete after X** |
| P7 | `internal/config/cost_tier.go` — `claudeSonnetPreset`, `claudeHaikuPreset`, `CostTierAgents`, `CostTierRoleAgents`, `ApplyCostTier`; `internal/config/loader.go` — `costTierPresetByName` | Builds host Claude CLI presets and maps the budget tier's crew to `claude-sonnet` | `costTierPresetByName` ← `lookupAgentConfigIfExists` ← `AgentResolver`, `ValidateAgentConfig`, `ResolveAgentConfigByName`; `ApplyCostTier` ← `gt config cost-tier` (`internal/cmd/config.go`); the tier tables ← `GetCurrentTier`/`FormatTierRoleTable` | Yes for crew and reviewer roles, not for a polecat seat. A polecat reaches the name only through P1–P5 | **Keep** |
| P8 | `internal/cmd/model.go` — `modelBuiltinProfiles` | Ships the `haiku`/`sonnet` profiles `gt model` reads and applies | `readModelView` and the `gt model` commands (`runModelShow`, `runModelSetRole`, `runModelSetDefault`, `runModelApply`) | Live as a host-role setting; the names resolve through P7 | **Keep** |
| P9 | `internal/cmd/pool_migrate.go` — `poolMigrateDefaultHookedAgent` | The second seat's default when a two-seat config set `max_hooked` and no `hooked_agent`; it was `claude-sonnet` | `takePatrolSeatKeys` ← `planPoolMigrate` ← `runPoolMigrate` (`gt pool migrate`) | Only a config that still holds the two-seat keys; this town's does not. `checkMigrated` needs the name to resolve, which P7 provides | **Keep** |
| P10 | `internal/cmd/pool.go` — help and `parsePoolSeatCaps` error; `internal/cmd/pool_mix.go` — signpost | `gt pool set` help/error examples use `claude-haiku`/`deepseek-flash`; `gt pool mix` is a retired signpost whose examples use them | Cobra registration; `runPoolMix` from the `pool mix` command | Text only: `gt pool mix` exits 2 for every argument except `-h`/`--help` | **Delete now (B4)** |
| P11 | `internal/cmd/sling_schedule.go` — `ScheduledSling.Agent`; `internal/daemon/scheduled_slings.go` — `--agent`; `internal/daemon/scheduled_slings_spec.go` — `run_agent` check | A non-spec scheduled entry carries an agent into `gt sling`; a spec entry's `agent` is refused at load and its `run_agent` must name a seat | `scheduleSlingBead` in `internal/cmd/sling_schedule.go`; the scheduler's `sling` construction in `internal/daemon/scheduled_slings.go` | Yes for a non-spec entry that still names a retired agent; the 0-label fact does not cover patrol entries | **Keep** (the field can name `sbx-*`; the refusal is B2) |
| P12 | `internal/beads/beads_agent.go` — `AgentFields.Agent` | Records the agent a sling spawned the seat on, for the roster and later restarts | Written by `internal/cmd/polecat_spawn.go` (`AddOptions`), read by the agent-bead/roster readers | Generic: holds `sbx-*` for the live seats | **Keep** |
| P13 | `internal/agentlog/transcript.go` — `claudeProjectDirsFor` and its `~/.claude` fallback | Searches every root that can hold a seat's transcript, including the default root a host agent inherits | `LatestTranscript`/`ProjectDirsFor` ← `internal/liveness`, `internal/jev`, `internal/daemon` | Host roles still log under `~/.claude`; a seat spawned with its own `CLAUDE_CONFIG_DIR` logs there | **Keep** |
| P14 | `internal/models/backend.go` — `ModelFamily`; `internal/cmd/dashboard_om.go` — `omModelName` | Shortens a model name to its family for display | `internal/cmd/spec.go` (overrun note), `internal/cmd/dashboard_models.go`, `internal/cmd/dashboard_om.go` | Display only; `flash` is still the family of `sbx-flash` | **Keep** |
| P15 | no code constructor: `deepseek-flash` is an `agents` entry in the live settings | The name resolves through the generic `lookupAgentConfigIfExists` and `ResolveAgentConfigWithOverride`; the code ships no preset for it | The resolver callers in P7; `ModelFamily` for display (P14) | Yes while the settings entry exists; a seat or `--agent deepseek-flash` then reaches P2 | **Keep** (the settings entry is the overseer's to clean; no code to delete) |

`delete after X` (P6) is one condition: the overseer confirms every `sandboxes`
profile in the live settings sets `driver: kubernetes`. Then one bead in
`internal/config` defaults an unset driver to kubernetes and refuses `docker`,
and one in `internal/session` deletes the Docker-volume path.

Five files the seat-name grep reaches have no path row because no live path
starts in them: `internal/cmd/polecat_spawn.go` is only a caller (of P2, P5 and
P12) and its two seat mentions are comments, while `internal/config/env.go`,
`internal/cmd/dashboard_trend.go`, `internal/daemon/spec_seats.go` and
`internal/daemon/spec_dispatch.go` name one in a comment only. B4, B5 and B6
sweep them as text.

## Deletion beads (delete now)

Each is sized to one package and each needs a test that is red when the path is
removed.

- **B1 — `internal/config`: "config: a polecat seat must name the openshell backend."**
  Deletes the `backend: ""`/`tmux` acceptance in `Seats` and the tmux value the
  seat loader returns. Test: a seat with no backend, or `backend: tmux`, is
  refused by name. Check before landing: the live `sbx-*` seats name
  `backend: openshell` (overseer), otherwise a live tmux seat is the first
  finding.
- **B2 — `internal/cmd`: "cmd: refuse a polecat agent that names no pool seat."**
  Deletes the tmux fallback in `sessionBackendForAgent` and the no-agent
  pass-through in `choosePoolAgent`; the requested name in a sling, a scheduled
  entry, and a crash restart all go through them. Tests: a sling `--agent` with
  no seat is refused, and a restart whose intent record names no seat is
  refused rather than host-run. The settings cleanup is not a substitute: a
  built-in preset such as `claude` still resolves after it, and the route, not
  the name, is what this bead closes.
- **B3 — `internal/polecat`: "polecat: a session start with no backend is an error."**
  Deletes the nil-backend host default in `SessionManager.Start`. Test: a start
  with no backend errors and creates no session.
- **B4 — `internal/cmd`: "cmd: replace the retired seat names in gt pool and gt model help and comments."**
  Text only: `pool.go`, `pool_mix.go`, `sling.go`, `sling_schedule.go`,
  `polecat_spawn.go`, `handoff.go`, `formula.go`, `rig_settings.go`,
  `pool_seats.go`, `dashboard_trend.go`. No live code path changes.
- **B5 — `internal/config`: "config: replace the retired seat names in comments and type examples."**
  Text only: `env.go`, `types.go` (role/crew agent examples), `loader.go`,
  `cost_tier.go` where a retired name is only an example.
- **B6 — `internal/daemon`: "daemon: replace the retired seat examples in the spec dispatcher comments."**
  Text only: `spec_seats.go`, `spec_dispatch.go`.
- **B7 — docs: "docs: replace the retired polecat seat names with the sandbox seats."**
  Text only, in the files listed under **Docs** below.

## Keep, with reason

- P7 and P8 are host-role features. Deleting the cost-tier presets deletes
  `gt config cost-tier budget`, not polecat debt; a polecat reaches them as a
  host run only through the paths B1–B3 close.
- P9 migrates a config that predates the seat map. It records the old
  dispatcher's default, not a seat, and it depends on P7 resolving
  `claude-sonnet`; if P7 is ever deleted, P9 must become a refusal.
- P11's field still names a seat for a scheduled sling (`--agent sbx-flash`).
- P12–P15 are generic to host roles and to the `sbx-*` seats; P15 has no code to
  delete, only the settings entry the overseer owns.

## Docs

The exact-name grep at `origin/main` (`65474d9e`,
`grep -rnE '(^|[^a-zA-Z0-9-])<name>([^a-zA-Z0-9-]|$)' docs/`):

| File | Lines | What the line is |
|---|---|---|
| `docs/INSTALLING.md` | 265, 268, 274: `claude-haiku` on each | Install examples: `gt config agent set`, `gt config default-agent`, `gt sling --agent` |
| `docs/reference.md` | 996, 1009, 1011, 1076: `claude-haiku`; 618: `deepseek-flash` | Agent-config examples, a secrets example, and a sling example |
| `docs/concepts/identity.md` | 267: `claude-sonnet` and `claude-haiku`; 279, 280: `claude-sonnet` | The model-comparison section and its `bd count --label=agent:` examples |
| `docs/examples/town-settings.example.json` | 22, 25: `deepseek-flash` | An agent entry with `--model deepseek-flash` |
| `docs/design/k8s-sandboxes.md` | 18: `deepseek-flash-sandbox-k8s` | The first k8s seat in a dated decision; the page also still calls the Docker-driver seat the control, which P6 retires |
| `README.md` (outside `docs/`) | 435: `deepseek-flash`; 462: `claude-sonnet` | Quick-start examples |

Ten `claude-sonnet-4-5` hits in a removed historical design record (git history
keeps it) are a model ID, not the seat name. The exact-name grep does not count
them, because `docs/plans`, `docs/research` and the historical `docs/design`
files are not maintained.

The `deepseek` provider, the spend metric and the `deepseek` OpenShell provider
are live; only the *seat* names retired. A grep for `deepseek` in `internal/` is
not this inventory's debt, and B4–B7 leave those lines alone.

## The risky edge

Three code paths accept a seat name with a different predicate: `SeatsKnownTo`
(settings `agents` plus built-in presets), `AgentResolver` (the full lookup,
including the cost-tier fallback), and `sessionBackendForAgent`, which passes
`nil` and accepts any name. So a hand-edited seat naming a retired agent can
load and resolve its backend while `gt pool set` refuses the same name. B1
closes that at the backend rather than at the name, which is why it comes before
the text sweeps. The settings cleanup remains the overseer's step: it removes
the entry `ResolveAgentConfigWithOverride` reads, so a retired name no longer
resolves for a sling, but `claude-haiku` and `claude-sonnet` still resolve
through the cost-tier fallback in `lookupAgentConfigIfExists`, and any agent
that resolves and has no seat still reaches host tmux through P3 and P2.

## Evidence

The greps, run at `origin/main` over the non-test tree:

- `grep -rn 'config\.Seats(' internal/ cmd/`
- `grep -rn 'sessionBackendForAgent' internal/cmd/`
- `grep -rn 'choosePoolAgent(' internal/cmd/`
- `grep -rn 'restartPolecatOnSlungAgent' internal/cmd/`
- `grep -rn 'SessionStartOptions\.Backend\|opts\.Backend' internal/polecat/`
- `grep -rn 'SandboxDriverDocker\|WithDefaults()\|usesDockerVolumes' internal/ cmd/`
- `grep -rn 'ApplyCostTier\|CostTierAgents\|costTierPresetByName' internal/ cmd/`
- `grep -rn 'modelBuiltinProfiles' internal/cmd/`
- `grep -rn 'poolMigrateDefaultHookedAgent' internal/cmd/`
- `grep -rn 'ScheduledSling.Agent\|--agent=' internal/cmd/ internal/daemon/`
- `grep -rn 'AgentFields' internal/beads/`
- `grep -rn 'claudeProjectDirsFor\|LatestTranscript\|ProjectDirsFor' internal/`
- `grep -rn 'ModelFamily(\|omModelName(' internal/ cmd/`
- the docs grep above, and `grep -rn -E 'claude-haiku|claude-sonnet|deepseek-flash' docs/ README.md`
