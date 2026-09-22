# Gas Town Plugins

This directory contains town-level plugins that run during Deacon patrol cycles.

## Plugin Structure

Each plugin is a directory containing:
- plugin.md - Plugin definition with TOML frontmatter

## Gate Types

- cooldown: Time since last run (e.g., 24h)
- cron: Schedule-based (e.g., "0 9 * * *")
- condition: Metric threshold
- event: Trigger-based (startup, heartbeat)

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
record it as a clean "no results" success. (Known affected: `compactor-dog`'s
"last run" check in `plugins/compactor-dog/plugin.md`.)

## Deployed copy

This `plugins/` directory (checked out at `<town_root>/gastown/mayor/rig/plugins`)
is the source of truth. Deacon and `gt doctor` patrols run plugins out of
`<town_root>/plugins` (e.g. `~/gt/plugins`) — a separate runtime copy that a
`git pull` alone does not update.

An edit made directly under `<town_root>/plugins` is a draft, not a change: the
next `gt plugin sync` overwrites it. Land the edit here first.

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
