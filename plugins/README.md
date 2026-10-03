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
(`[execution] type = "script"` with a `run.sh`) in-process, recording a
plugin-run receipt. A failed run is logged and escalated with `gt escalate`
under the fingerprint `plugin:<name>:failed`; the plugin's next good run
(success or skipped) closes it. Nothing runs markdown-instruction ("agent")
plugins any more: a cooldown plugin without a script `run.sh` is logged as
skipped every heartbeat. No patrol schedules plugins: one the daemon dispatches runs twice if
a patrol step also runs it (gt-o1z7).

A `manual` gate parks a plugin off the automatic path, by design: the daemon
prints one skip line per heartbeat while it stays parked, and the only way to
run it is `gt plugin run <name>`. For a script-type plugin this runs `run.sh`
directly and records the real result; for any other plugin it prints the
instructions and nothing more, so whoever runs it executes the instructions
and records the result (`gt plugin record-run`) (gt-o1z7).

`cron`, `condition` and `event` are parsed but nothing dispatches them
(gt-qehkn); a plugin on one of these gates has no automatic path today.
`gt plugin run <name>` still works on them — for a script-type plugin it runs
`run.sh` directly, for any other type it prints instructions — same as a
manual gate, minus the cooldown check.

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

One caller pushes this directory to the runtime copy, and it is not a
guarantee that the runtime copy is current:

- `make install` (`scripts/install-gt.sh`) runs `gt plugin sync` from
  `<town>/gastown/mayor/rig` after every successful install. The step is
  fail-open: a failed sync does not fail the install, and it is logged as
  "non-fatal" rather than discarded.

The `rebuild-gt` plugin ran the same script too, once; it is a daemon job in
Go now (internal/daemon/rebuild_gt.go, gt-4k3fj.8.6).

`gt plugin sync` resolves the town root from the CWD, so that path fails
outright when this checkout lives outside the town root (a `LocalRepo`
override). `gt doctor`'s `patrol-plugin-drift` check is what catches the
resulting divergence: it compares the two copies and warns when they diverge,
including a plugin the runtime copy still has and this directory no longer does
(deleting a plugin directory here does not remove its runtime copy until
something prunes it), or when it cannot locate this source directory at all —
it never silently reports OK in that case. `gt doctor fix patrol-plugin-drift
--authorized-by <bead-id>` rewrites the runtime copy from this directory and
prunes those extras; it removes directories, so an agent needs the recorded
authorization gt-638go.3 requires. A runtime plugin holding edits this
directory never had is left in place, and the repair fails rather than
reporting success.
