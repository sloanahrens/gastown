+++
name = "rebuild-bd"
description = "Rebuild stale bd binary from beads fork source, with post-install smoke test and automatic rollback"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:rebuild-bd", "rig:beads", "category:maintenance"]
digest = true

[execution]
timeout = "10m"
notify_on_failure = true
severity = "high"
+++

# Rebuild bd Binary

Checks if the town-wide `bd` binary is stale relative to the beads fork's
`origin/main` and, if so, rebuilds and installs it — then proves the new
binary actually works before leaving it in place.

**SAFETY**: bd is the DATA PLANE for every agent in the town (gt, mayor,
deacon, witnesses, refineries, every polecat read and write beads through
it). A bad `gt` binary caused a documented crash loop (see
`plugins/rebuild-gt/plugin.md`); a bad `bd` binary is strictly worse,
because every agent loses its issue tracker simultaneously and the
recovery agents themselves are impaired. This plugin is therefore MORE
conservative than `rebuild-gt`, not merely equivalent:

- It reuses `rebuild-gt`'s safety rails verbatim (cooldown gate, forward-only,
  main-only, dirty guard ignoring untracked files and `.beads/` churn,
  self-serve `git fetch` + `merge --ff-only` that SKIPS on divergence rather
  than forcing).
- It does NOT hand-roll the forward-only ancestor check — that logic lives in
  the beads Makefile's `check-forward-only`/`safe-install` targets (added by
  be-suy) and is reused here via `make`.
- It ADDS a post-install smoke test with automatic rollback, which
  `rebuild-gt` has none of. `rebuild-gt` only replaces the binary; if the new
  `gt` were broken, a human would notice via crash-loop symptoms. Nothing
  would ever notice a broken `bd` that fast, because `bd` failures look like
  "Dolt is having trouble" to every agent that hits them independently —
  so this plugin verifies the binary works before declaring success.
- It ADDS a rollback copy of the outgoing binary, saved before every
  overwrite, so rollback never depends on rebuilding from source or on
  Homebrew still being reachable.

## Gate Check

The Deacon evaluates this before dispatch. If gate closed, skip.

## Drift Escalation

Every skip path below (dirty repo, wrong branch, diverged local main, "not
safe to rebuild") is a normal, expected outcome on its own — but any one of
them can persist for hours while the binary quietly falls behind
`origin/main`, and none of the individual skip checks would ever notice that
on their own (see gt-bce, the equivalent gap in `rebuild-gt`). There is no
`bd`-equivalent of `gt stale --json`, so this is computed directly with git
against the beads rig clone before any pre-flight check runs:

```bash
cd ~/gt/beads/mayor/rig
git fetch origin main --quiet
git rev-list --count HEAD..origin/main
```

If the count exceeds a threshold (default 20, override with
`REBUILD_BD_MAX_COMMITS_BEHIND`), escalate with a stable fingerprint so
repeated runs don't spam duplicate escalations while the condition persists:

```bash
gt escalate "rebuild-bd: binary is $N commits behind origin/main and has not been rebuilt" \
  -s medium \
  --source "plugin:rebuild-bd" \
  --fingerprint "rebuild-bd:drift" >/dev/null 2>&1 || true
```

## Pre-flight Checks

Before touching anything, verify the beads rig clone is clean and on main:

```bash
cd ~/gt/beads/mayor/rig
git status --porcelain --untracked-files=no -- . ':(exclude).beads'  # No tracked changes
git branch --show-current  # Must be "main"
```

If either check fails, skip the rebuild and record a skip wisp.

## Sync with origin/main

```bash
cd ~/gt/beads/mayor/rig
git merge --ff-only origin/main --quiet   # origin/main already fetched above
```

`--ff-only` is load-bearing: if local `main` has diverged from
`origin/main`, the merge fails and the plugin must skip the rebuild and
record a skip wisp with reason "local main diverged from origin/main" —
**never** `git reset --hard` to force it.

## Detection (forward-only, delegated to the Makefile)

```bash
cd ~/gt/beads/mayor/rig && make check-forward-only
```

This target (added by be-suy) reads the installed binary's commit via
`bd version --json` and compares it against `HEAD` with
`git merge-base --is-ancestor`:

- Exits 1 with `"already at HEAD"` in the output → binary is fresh. Record
  success and exit 0. Nothing else to do.
- Exits 1 with `"DOWNGRADE"` in the output → should not happen after a
  successful `--ff-only` merge, but if it does, record a skip (never force)
  and exit 0.
- Exits 0 (including the "no prior fork build, skipping forward check"
  warning path for the very first install) → proceed.

Do not reimplement this ancestor comparison — `check-forward-only` already
does exactly what `gt stale`'s `safe_to_rebuild` field does for `gt`.

## Backup Before Overwrite

Before installing, preserve the outgoing binary so rollback never depends on
rebuilding or on Homebrew:

```bash
INSTALL_DIR="$HOME/.local/bin"
if [ -f "$INSTALL_DIR/bd" ]; then
  rm -f "$INSTALL_DIR"/bd.prev-*         # keep exactly one rollback copy
  BACKUP="$INSTALL_DIR/bd.prev-$(date +%Y%m%d%H%M%S)"
  cp -p "$INSTALL_DIR/bd" "$BACKUP"
fi
```

If no binary exists yet at `$INSTALL_DIR/bd` (very first fork install),
there is nothing to back up — rollback for that case means removing the new
binary to un-shadow Homebrew, not restoring a copy.

## Action

```bash
cd ~/gt/beads/mayor/rig && make safe-install
```

`safe-install` (added by be-suy) chains `check-up-to-date`, `check-on-main`,
`check-forward-only`, `build`, then an atomic install that also recreates the
`beads` alias symlink. It does not restart anything — bd has no daemon.

## Smoke Test (REQUIRED — this is what makes rebuild-bd safe to run unattended)

```bash
INSTALL_DIR="$HOME/.local/bin"
"$INSTALL_DIR/bd" version
( cd ~/gt/beads/mayor/rig && "$INSTALL_DIR/bd" list --limit 1 )
```

Both must exit 0. The second is a *real read against a live rig DB* — a
binary that starts but can't talk to Dolt is exactly the failure mode this
plugin exists to catch before it reaches the rest of the town.

**On smoke-test failure**: restore `$BACKUP` if one was made; otherwise
remove `$INSTALL_DIR/bd` and `$INSTALL_DIR/beads` entirely so Homebrew's bd
resumes serving the PATH. Verify the restored/removed state actually leaves
a working `bd version` before declaring the rollback complete. Record a
failure wisp and escalate at `-s high` (`-s critical` if the rollback
verification itself fails — that means the town has no working `bd` and
nothing is fixing it automatically).

## Record Result

On success (real install + smoke test passed):
```bash
gt plugin record-run --plugin rebuild-bd --result success --rig beads \
  --title "Plugin: rebuild-bd [success]" \
  --description "Rebuilt bd: $OLD -> $NEW (smoke test passed; previous binary at $BACKUP)" >/dev/null 2>&1 || true
```

On fresh (no-op):
```bash
gt plugin record-run --plugin rebuild-bd --result success --rig beads \
  --title "rebuild-bd: binary is fresh" >/dev/null 2>&1 || true
```

On skip (dirty, wrong branch, diverged, downgrade guard tripped):
```bash
gt plugin record-run --plugin rebuild-bd --result skipped --rig beads \
  --title "Plugin: rebuild-bd [skipped]" \
  --description "Skipped: <reason>" >/dev/null 2>&1 || true
```

On failure (build failed, or smoke test failed and rollback occurred):
```bash
gt plugin record-run --plugin rebuild-bd --result failure --rig beads \
  --title "Plugin: rebuild-bd [failure]" \
  --description "<what failed, and what the rollback did>" >/dev/null 2>&1 || true

gt escalate "rebuild-bd: <what failed>" -s high --source "plugin:rebuild-bd" 2>/dev/null || true
```

## Deployment

Adding this directory to the gastown repo does NOT make it run. The live
plugin directory `~/gt/plugins/` is a deployed copy, not synced automatically
from the repo — it must also be placed at `~/gt/plugins/rebuild-bd/` (and
kept current the same way `rebuild-gt` and other plugins are, via
`gt plugin sync` after a `rebuild-gt` cycle, or a manual copy until then).

## Homebrew Interaction

`~/.local/bin` sits before `/opt/homebrew/bin` on every agent's `PATH`
(verified: position 2 vs position 7), so installing to
`~/.local/bin/bd` genuinely shadows Homebrew — the switchover mechanism is
PATH precedence, not uninstalling anything. Homebrew's `bd` remains
installed at `/opt/homebrew/bin/bd` as a standing fallback; nothing in this
plugin touches it. The `tool-updater` plugin upgrades `beads` via Homebrew
on a weekly cadence purely for its own bookkeeping (an outdated check +
`brew upgrade`) — once shadowed, that upgrade becomes pointless but harmless;
`tool-updater` does not compare against the resolved `bd` binary, so it
neither warns nor fights with this plugin.
