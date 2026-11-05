# app-promote

Before changing what the plugin promotes or reading a failure it raised, read
`plugin.md` beside this file: it defines the gate (GitHub follows prod), what the
plugin refuses and how it reports a failure. This file covers installing it.

## What it needs

- **A viewer token.** `~/.config/gt/forgejo-viewer.env` holding
  `FORGEJO_TOKEN=...`, mode 600 — the read-only token the dashboard's Forgejo
  panels read, `internal/forgejo/token.go` for the rule. The plugin only GETs.
- **A promote target per rig.** The rig's `merge_queue.forgejo` block
  (`<rig>/settings/config.json`, the operator tier) must name `promote_target`
  and `promote_key_file`, or `gt promote` refuses every sha and the plugin fails
  its run naming the rig. `bash scripts/forgejo-cutover.sh <rig>` writes both.
- **The app repo's `deploy.yml`** with the prod integration job `plugin.md`
  names. A repo whose `deploy.yml` lacks it is never promoted.

## Installing it, and adding a rig

The plugin runs from `~/gt/plugins/`, which `gt plugin sync` fills from the
gastown checkout's `plugins/`. Before building or syncing it, read "Script
entry point" in `plugins/README.md`; it states why a plugin that names a binary
must build it before a sync. Sync, then confirm:

```bash
gt plugin sync --dry-run     # see what would change
gt plugin sync
gt plugin show app-promote   # the gate, the labels, the path it runs from
```

The daemon runs it on its next heartbeat once the 10-minute cooldown gate is
open — nothing else starts it. Adding a rig is one line in `rigs.conf` and
another sync; edit that file in the gastown checkout, not in `~/gt/plugins/`,
because a sync leaves a town copy carrying content the repo never had untouched,
lists it and exits non-zero (gt-o848l).

## Running it, and reading what it did

```bash
~/gt/plugins/app-promote/bin/app-promote
```

A run that promoted prints one line per rig; one with nothing to promote prints
`[plugin-result skipped]`, which is what the daemon records as skipped instead of
successful. The daemon keeps the last run's whole output at
`~/gt/daemon/plugin-runs/app-promote.log`, escalates a failure under
`plugin:app-promote:failed` with the output tail, and closes that escalation on
the next clean run. `gt plugin history app-promote` lists the receipts.

A `gt promote` refusal fails the run unless it is a routine state the next run
resolves on its own. Routine states are logged with `gt promote`'s own line, and
the rig counts as checked:

- exit 1, the commit is not on the rig's Forgejo `main` yet;
- exit 1, the tier sweep owns the promotion and has no fully green cycle yet;
- exit 1, another promotion holds the rig's lock — retry later;
- exit 2, the promotion target diverged and `gt promote` has already escalated it.

Every other non-zero exit is a failure: a rig that names no `promote_target` or
`promote_key_file`, a rejected push, a `gt promote` that cannot start, or a
message this list does not name. One failing rig does not stop the others: the
run checks every rig in `rigs.conf`, then fails naming each failed rig with its
exit code and the cause line, one line each. A run with any failure never prints
`[plugin-result skipped]`, even when no rig promoted.

To retry one promotion by hand, run the command the plugin runs — the exit codes
and refusals are in `gt promote --help`:

```bash
gt promote --rig fractals --sha <commit>
```

## Setting a rig aside

Removing a rig from `rigs.conf` and syncing stops promoting for it and leaves
every other rig alone. `--clean` on the sync is what removes a plugin the
checkout no longer holds; it takes the whole plugin, not one rig.
