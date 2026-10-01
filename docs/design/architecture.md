# Gas Town Architecture

Technical architecture for Gas Town multi-agent workspace management.

## Two-Level Beads Architecture

Gas Town uses a two-level beads architecture to separate organizational coordination
from project implementation work.

| Level | Location | Prefix | Purpose |
|-------|----------|--------|---------|
| **Town** | `~/gt/.beads/` | `hq-*` | Cross-rig coordination, Mayor mail, agent identity |
| **Rig** | `<rig>/mayor/rig/.beads/` | project prefix | Implementation work, landing records, project issues |

### Town-Level Beads (`~/gt/.beads/`)

Organizational chain for cross-rig coordination:
- Mayor mail and messages
- Convoy coordination (batch work across rigs)
- Strategic issues and decisions
- **Town-level agent beads** (Mayor)
- **Role definition beads** (global templates)

### Rig-Level Beads (`<rig>/mayor/rig/.beads/`)

Project chain for implementation work:
- Bugs, features, tasks for the project
- Landing records and review verdicts, on the work beads
- Project-specific molecules
- **Rig-level agent beads** (Polecats, Crew)

## Agent Bead Storage

Agent beads track lifecycle state for each agent. Storage location depends on
the agent's scope.

| Agent Type | Scope | Bead Location | Bead ID Format |
|------------|-------|---------------|----------------|
| Mayor | Town | `~/gt/.beads/` | `hq-mayor` |
| Polecats | Rig | `<rig>/.beads/` | `<prefix>-<rig>-polecat-<name>` |
| Crew | Rig | `<rig>/.beads/` | `<prefix>-<rig>-crew-<name>` |

### Role Beads

Role beads are global templates stored in town beads with `hq-` prefix:
- `hq-mayor-role` - Mayor role definition
- `hq-polecat-role` - Polecat role definition
- `hq-crew-role` - Crew role definition

Each agent bead references its role bead via the `role_bead` field.

## Agent Taxonomy

### Town-Level Agents (Cross-Rig)

| Agent | Role | Persistence |
|-------|------|-------------|
| **Mayor** | Global coordinator, handles cross-rig communication and escalations | Persistent |

### Rig-Level Agents (Per-Project)

| Agent | Role | Persistence |
|-------|------|-------------|
| **Polecats** | Workers with persistent identity, assigned to specific issues | Persistent identity, ephemeral sessions |
| **Crew** | Human workspaces — full git clones, user-managed lifecycle | Persistent |

### The Daemon

The daemon is a Go process, not an agent. It is the only process that kills or
restarts a session ([ADR 0003](../adr/0003-one-supervisor-no-idle-llm.md)), runs
the per-rig `patrol_scan` tick that restarts a dead polecat holding work
([ADR 0005](../adr/0005-patrol-scan-tick.md)), and hosts the per-rig landing
worker ([ADR 0004](../adr/0004-daemon-lands-work.md)).

## Directory Structure

```
~/gt/                           Town root
├── .beads/                     Town-level beads (hq-* prefix)
│   ├── metadata.json           Beads config (dolt_mode, dolt_database)
│   └── routes.jsonl            Prefix → rig routing table
├── .dolt-data/                 Centralized Dolt data directory
│   ├── hq/                     Town beads database (hq-* prefix)
│   ├── gastown/                Gastown rig database (gt-* prefix)
│   ├── beads/                  Beads rig database (bd-* prefix)
│   └── <other rigs>/           Per-rig databases
├── .runtime/                   Town runtime state
│   └── landings/<rig>.jsonl    Append-only landings file, written by the landing worker
├── daemon/                     Daemon runtime state
│   ├── dolt-state.json         Dolt server state (pid, port, databases)
│   ├── dolt-server.log         Server log
│   └── dolt.pid                Server PID file
├── mayor/                      Mayor agent home
│   ├── town.json               Town configuration
│   ├── rigs.json               Rig registry
│   ├── daemon.json             Daemon patrol config
│   └── accounts.json           Claude Code account management
├── settings/                   Town-level settings
│   ├── config.json             Town settings (agents, themes)
│   └── escalation.json         Escalation routes and contacts
├── directives/                 Town-level role directives (operator policy)
│   └── <role>.md               Markdown injected at prime time
├── formula-overlays/           Town-level formula overlays
│   └── <formula>.toml          TOML step overrides (replace/append/skip)
├── config/
│   └── messaging.json          Mail lists, queues, channels
└── <rig>/                      Project container (NOT a git clone)
    ├── config.json             Rig identity and beads prefix
    ├── directives/             Rig-level role directives (overrides town)
    │   └── <role>.md
    ├── formula-overlays/       Rig-level formula overlays (full precedence)
    │   └── <formula>.toml
    ├── mayor/rig/              Canonical clone (beads live here, NOT an agent)
    │   └── .beads/             Rig-level beads (redirected to Dolt)
    ├── crew/                   Crew parent
    │   └── <name>/             Human workspaces (full clones)
    └── polecats/               Polecats parent
        └── <name>/<rigname>/   Worker worktrees from mayor/rig
```

**Note**: No per-directory CLAUDE.md or AGENTS.md is created. Only `~/gt/CLAUDE.md`
(town-root identity anchor) exists on disk. Full context is injected by `gt prime`
via SessionStart hook.

### Worktree Architecture

Polecats are git worktrees, not full clones. This enables fast spawning
and shared object storage. The worktree base is `mayor/rig`:

```go
// From polecat/manager.go - worktrees are based on mayor/rig
git worktree add -b polecat/<name>-<timestamp> polecats/<name>
```

Crew workspaces (`crew/<name>/`) are full git clones for human developers who need
independent repos. Polecat sessions are ephemeral and benefit from worktree efficiency.

## Storage Layer: Dolt SQL Server

All beads data is stored in a single Dolt SQL Server process per town. There is
no embedded Dolt fallback — if the server is down, `bd` fails fast with a clear
error pointing to `gt dolt start`.

```
┌─────────────────────────────────┐
│  Dolt SQL Server (per town)     │
│  Port 3307, managed by daemon   │
│  Data: ~/gt/.dolt-data/         │
└──────────┬──────────────────────┘
           │ MySQL protocol
    ┌──────┼──────┬──────────┐
    │      │      │          │
  USE hq  USE gastown  USE beads  ...
```

Each rig database is a subdirectory under `.dolt-data/`. The daemon monitors
the server on every heartbeat and auto-restarts on crash.

For write concurrency, all agents write directly to `main` using transaction
discipline (`BEGIN` / `DOLT_COMMIT` / `COMMIT` atomically). This eliminates
branch proliferation and ensures immediate cross-agent visibility.

See [dolt-storage.md](dolt-storage.md) for full details.

## Beads Routing

The `routes.jsonl` file maps issue ID prefixes to rig locations (relative to town root):

```jsonl
{"prefix":"hq-","path":"."}
{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"bd-","path":"beads/mayor/rig"}
```

Routes point to `mayor/rig` because that's where the canonical `.beads/` lives.
This enables transparent cross-rig beads operations:

```bash
bd show hq-mayor    # Routes to town beads (~/.gt/.beads)
bd show gt-xyz      # Routes to gastown/mayor/rig/.beads
```

## Beads Redirects

Worktrees (polecats, crew) don't have their own beads databases. Instead,
they use a `.beads/redirect` file that points to the canonical beads location:

```
polecats/alpha/.beads/redirect → ../../mayor/rig/.beads
```

`ResolveBeadsDir()` follows redirect chains (max depth 3) with circular detection.
This ensures all agents in a rig share a single beads database via the Dolt server.

## Landing: The Daemon Lands Work

Work reaches main only through the daemon's landing worker
([ADR 0004](../adr/0004-daemon-lands-work.md)). There is no merge queue,
batching or bisection.

```
Polecat or crew finishes work
  → gt done: fetch, rebase onto target, squash checkpoints, local gate
  → Push branch under a lease, read the tip back
  → Label the work bead gt:ready-to-land
Landing worker (one per rig, serial within a rig)
  → Throwaway worktree at origin/<target>, merge the branch
  → Gate and review the merged tree concurrently
  → Push with --force-with-lease, read the tip back
  → Close the bead with the landing record; append to the landings file
```

A rejection is written to the work bead and the bead goes back for rework.
Polecats never push main. Polecats manage their own lifecycle up to `gt done`;
the daemon restarts a session only when it is dead while holding work.

## Data Plane Lifecycle

All beads data flows through a six-stage lifecycle managed by the daemon:

```
CREATE → LIVE → CLOSE → DECAY → COMPACT → FLATTEN
  │        │       │        │        │          │
  Dolt   active   done   DELETE   REBASE     SQUASH
  commit  work    bead    rows    commits    all history
                         >7-30d  together   to 1 commit
```

Stages 1-3 are automated today. Stages 4-6 run as daemon patrols
(gt-at0i Reaper DELETE, gt-l8dc Compactor REBASE, gt-emm4 Doctor gc).

These maintenance patrols (`doctor_dog`, `wisp_reaper`, `compactor_dog`,
`checkpoint_dog`, `jsonl_git_backup`, `scheduled_maintenance`) are imperative Go inside the
daemon: a ticker fires and the daemon does the work itself, with no agent
involved. Jobs that pour a `mol-dog-*` molecule pour it only as an observability
receipt; nothing picks the molecule up. The `_dog` suffix is historical — the
LLM dog role that once ran such formulas was retired. Reliability-critical
maintenance stays in the daemon; opportunistic work belongs in a script plugin
(see [plugin-system.md](plugin-system.md)).

See [dolt-storage.md](dolt-storage.md) for full details.

## Deployment Artifacts

Gas Town and Beads are distributed through multiple channels. Tag pushes (`v*`)
trigger GitHub Actions release workflows that build and publish everything.

### Gas Town (`gt`)

| Channel | Artifact | Trigger |
|---------|----------|---------|
| **GitHub Releases** | Platform binaries (darwin/linux/windows, amd64/arm64) + checksums | GoReleaser on tag push |
| **Homebrew** | `brew install steveyegge/gastown/gt` — formula auto-updated on release | `update-homebrew` job pushes to `steveyegge/homebrew-gastown` |
| **npm** | `npx @gastown/gt` — wrapper that downloads the correct binary | OIDC trusted publishing (no token) |
| **Local build** | `go build -o $(go env GOPATH)/bin/gt ./cmd/gt` | Manual |

### Beads (`bd`)

| Channel | Artifact | Trigger |
|---------|----------|---------|
| **GitHub Releases** | Platform binaries + checksums | GoReleaser on tag push |
| **Homebrew** | `brew install steveyegge/beads/bd` | `update-homebrew` job |
| **npm** | `npx @beads/bd` — wrapper that downloads the correct binary | OIDC trusted publishing (no token) |
| **PyPI** | `beads-mcp` — MCP server integration | `publish-pypi` job with `PYPI_API_TOKEN` secret |
| **Local build** | `go build -o $(go env GOPATH)/bin/bd ./cmd/bd` | Manual |

### npm Authentication

Both repos use **OIDC trusted publishing** — no `NPM_TOKEN` secret needed.
Authentication is handled by GitHub's OIDC provider. The workflow needs:

```yaml
permissions:
  id-token: write  # Required for npm trusted publishing
```

Configure on npmjs.com: Package Settings → Trusted Publishers → link to the
GitHub repo and `release.yml` workflow file.

### What the binary embeds

The Go binary is the primary distribution vehicle. It embeds:
- **Role templates** — Agent priming context, served by `gt prime`
- **Formula definitions** — Workflow molecules, served by `bd mol`
- **Doctor checks** — Health diagnostics, including migration checks
- **Default configs** — `daemon.json` lifecycle defaults, operational thresholds

This means upgrading the binary automatically propagates most fixes. Files that
are NOT embedded (and require `gt doctor` or `gt upgrade` to update):
- Town-root `CLAUDE.md` (created at `gt install` time)
- `daemon.json` patrol entries (created at install, extended by `EnsureLifecycleDefaults`)
- Claude Code hooks (`.claude/settings.json` managed sections)
- Dolt schema (migrations run on first `bd` command after upgrade)

## Role Directives and Formula Overlays

Operators can customize agent behavior at the town or rig level without
modifying the Go binary or embedded templates. This follows the property layer
model (rig > town > system) and the hooks override precedent.

### Role Directives

Per-role Markdown files injected during `gt prime`, after the role template but
before context files and handoff content. Operator policy that overrides formula
instructions where they conflict.

```
~/gt/directives/_common.md             # Town-level, every role
~/gt/directives/<role>.md              # Town-level (all rigs)
~/gt/<rig>/directives/_common.md       # Rig-level, every role in that rig
~/gt/<rig>/directives/<role>.md        # Rig-level
```

Every file that exists concatenates, broadest first, so the most specific
appears last and wins conflicts. A file named for no role loads for no one:
`gt directive list` marks it `UNUSED (no such role)` and `gt doctor` warns.
Implemented in `internal/config/directives.go` (`LoadRoleDirective`,
`ScanDirectiveFiles`), integrated via `outputRoleDirectives()` in
`internal/cmd/prime_output.go`.

### Formula Overlays

Per-formula TOML files that modify individual steps. Applied post-parse before
rendering in `showFormulaStepsFull()`.

```
~/gt/formula-overlays/<formula>.toml   # the one overlay dir
```

A rig-level `~/gt/<rig>/formula-overlays/` dir is not read; `gt doctor`
reports any file left there. Three override modes:

| Mode | Effect |
|------|--------|
| `replace` | Swap the step description entirely |
| `append` | Add text after the existing step description |
| `skip` | Remove the step (dependents inherit its needs) |

bd applies the overlay: gt points `BD_FORMULA_OVERLAY_DIR` at the town's
overlay dir on every cook and bond, so prime renders and pour creates the same
text (gt-fd2cu.1). `internal/formula/overlay.go` (`LoadFormulaOverlay`) only
serves `gt formula overlay` and `gt doctor`, which validates overlay step IDs
against current formula definitions and can auto-fix stale references.

See [directives-and-overlays.md](directives-and-overlays.md) for the full
reference with examples and design rationale.

## See Also

- [dolt-storage.md](dolt-storage.md) - Dolt storage architecture
- [reference.md](../reference.md) - Command reference
- [directives-and-overlays.md](directives-and-overlays.md) - Directives and overlays reference
- [molecules.md](../concepts/molecules.md) - Workflow molecules
- [identity.md](../concepts/identity.md) - Agent identity and BD_ACTOR
