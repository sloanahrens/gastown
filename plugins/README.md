# Gas Town Plugins

This directory contains town-level plugins. The daemon heartbeat dispatches
them; see Scheduling below.

## Plugin Structure

Each plugin is a directory containing:
- plugin.md - Plugin definition with TOML frontmatter

## Gate Types

- cooldown: Time since last run (e.g., 24h)
- cron: Schedule-based (e.g., "0 9 * * *")
- condition: Metric threshold
- event: Trigger-based (startup, heartbeat)

## Scheduling

The daemon heartbeat is the only scheduler: it reads every plugin's gate on
each heartbeat and runs the ones whose gate is open — a script-type plugin
in-process (no dog session below the failure path, gt-fo2k), an agent-type one
as a dog. No patrol schedules plugins: one the daemon dispatches runs twice if
a patrol step also runs it (gt-o1z7).

A `manual` gate parks a plugin off the automatic path, by design: the daemon
prints one skip line per heartbeat while it stays parked, and the only way to
run it is `gt plugin run <name>` — which prints the plugin's instructions and
nothing more, so whoever runs it executes the instructions and records the
result (`gt plugin record-run`) (gt-o1z7).

`cron`, `condition` and `event` are parsed but nothing dispatches them
(gt-qehkn); a plugin on one of these gates has no automatic path today.

## Agent routing

Before writing a plugin whose job needs a model smarter than the dog default,
read `docs/design/plugin-system.md` for the optional top-level
`agent = "<preset>"` key. It runs that plugin's dog session on the named preset
instead of `role_agents.dog`; omit it and every plugin keeps sharing the role
default. `gt plugin show <name>` prints the preset a plugin resolves to.

## Querying your own run receipts

Plugin run receipts are **ephemeral wisps** (created by `gt plugin record-run`).
`bd list` hides ephemeral beads by design, so a plain query for them silently
returns `[]` even when receipts exist:

```bash
# WRONG — returns [] because the wisps are hidden by default
bd list --json --all -l type:plugin-run,plugin:<name>

# RIGHT — add --include-infra to surface the wisps
bd list --json --all --include-infra -l type:plugin-run,plugin:<name>
```

Prefer `gt plugin history <name> --json`, which already applies the correct
flags internally (see `internal/plugin/recording.go`).

**An empty result from such a query is not proof that nothing happened.** If a
plugin's own history shows recent receipts but a Step-1-style query returns `[]`,
the query is broken — treat that as a failed measurement and escalate; do not
record it as a clean "no results" success.

## Deployed copy

This `plugins/` directory (checked out at `<town_root>/gastown/mayor/rig/plugins`)
is the source of truth. The daemon runs plugins out of
`<town_root>/plugins` (e.g. `~/gt/plugins`) — a separate runtime copy that a
`git pull` alone does not update.

An edit made directly under `<town_root>/plugins` is a draft, not a change: the
next `gt plugin sync` overwrites it. Land the edit here first. A gate edited
there is a silent park (gt-o1z7).

Two paths push this directory to the runtime copy, and neither is a guarantee
that the runtime copy is current:

- `make install` runs `gt plugin sync --source $(CURDIR)/plugins` (Makefile:158).
  The step is fail-open: a failed sync does not fail the install. It is no
  longer silent — the failure is reported on stdout rather than discarded.
- The `rebuild-gt` plugin runs the same sync after every successful rebuild and
  logs a failure as "non-fatal".

`gt plugin sync` resolves the town root from the CWD, so both paths fail
outright when this checkout lives outside the town root (a `LocalRepo`
override). `gt doctor`'s `patrol-plugin-drift` check is what catches the
resulting divergence: it compares the two copies and warns when they diverge, or
when it cannot locate this source directory at all — it never silently reports
OK in that case.
