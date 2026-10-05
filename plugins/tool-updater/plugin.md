+++
name = "tool-updater"
description = "Report-only: list outdated Homebrew tools (dolt, beads); upgrades stay manual"
version = 1

[gate]
type = "manual"
# Manual by owner decision 2026-10-05 (gt-th5it). Reason and the manual upgrade
# step: "Upgrades are manual" below.

[tracking]
labels = ["plugin:tool-updater", "category:maintenance"]
digest = true

[execution]
type = "script"
+++

# Tool Updater

Reports which Homebrew tools (`dolt`, `beads`) are outdated, and changes
nothing.

Run it directly:

```bash
bash ~/gt/plugins/tool-updater/run.sh
```

## Upgrades are manual

Upgrading `dolt` here is `brew upgrade dolt` by hand, as a planned operation:
upgrading it via Homebrew swaps the engine binary on disk under the town's
running production Dolt server, so the server is restarted deliberately, not
under a heartbeat. Updating `beads` via Homebrew has nothing to fix, because the
town runs its own `bd` fork build at `~/.local/bin/bd`; the report marks it as
no action. `gt` is rebuilt separately by the daemon's `rebuild_gt` job, from
source (internal/daemon/rebuild_gt.go).
