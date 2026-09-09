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

## Deployed copy

This `plugins/` directory (checked out at `<town_root>/gastown/mayor/rig/plugins`)
is the source of truth. Deacon and `gt doctor` patrols run plugins out of
`<town_root>/plugins` (e.g. `~/gt/plugins`) — a separate runtime copy that
`git pull` and `make install` do **not** update.

Keep the runtime copy current with `gt plugin sync` (the `rebuild-gt` plugin
runs this automatically after every successful rebuild). `gt doctor`'s
`patrol-plugin-drift` check compares the two copies and warns when they
diverge, or when it cannot locate this source directory at all — it never
silently reports OK in that case.
