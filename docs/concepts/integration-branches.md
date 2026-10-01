# Integration Branches

> Let an epic's children build on each other on a shared branch.

An integration branch is a shared branch for one epic. A polecat slung a child
of that epic spawns its worktree from the integration branch, so it starts with
the sibling work already landed there, and its `gt done` targets the
integration branch instead of main.

No `gt` command creates, reports on or lands an integration branch. The
`gt mq integration` commands went with the merge queue (ADR 0004), and
integration landing was removed before that because it never landed anything
(gt-fcxe9.4). Moving the work from an integration branch to main is a human
decision made outside gt, and the pre-push hook blocks the obvious ways of
doing it by accident (see [Safety Guardrails](#safety-guardrails)).

## How a Branch Is Found

When `gt sling` spawns a polecat, it walks the hooked bead's parent chain (up
to 10 levels) looking for an epic with an integration branch:

| Step | Action |
|------|--------|
| 1 | For each ancestor epic, read `integration_branch:` from its metadata |
| 2 | Otherwise build a name from the default template `integration/{title}` (legacy: `integration/{epic}`) |
| 3 | Check that the branch exists, locally and then on origin |
| 4 | If found, spawn the worktree from it and pass it to the formula as `base_branch` |

`gt done` rebases onto `base_branch` and marks the bead ready to land on it;
the landing worker lands it there like any other target.

`{title}` is the epic title sanitized for git: lowercase, non-alphanumerics
turned into hyphens, at most 60 characters.

## Configuration

### Default Branch

The rig's `default_branch` (set in `config.json`, auto-detected during
`gt rig add`) is where work lands when no integration branch applies. If your
project uses `develop` or `master`, set it once in rig config:

```json
{
  "type": "rig",
  "name": "myproject",
  "default_branch": "develop"
}
```

### Polecat Sourcing

```json
{
  "merge_queue": {
    "integration_branch_polecat_enabled": true
  }
}
```

`integration_branch_polecat_enabled` (rig settings, `settings/config.json`)
controls whether polecats spawn from integration branches. Omitted or `null`
means true; set it to `false` to disable. `integration_branch_refinery_enabled`,
`integration_branch_template` and `integration_branch_auto_land` were removed
(gt-5nlvq): nothing read them.

## Safety Guardrails

The `.githooks/pre-push` hook detects when a push to the default branch
introduces integration branch content. It uses ancestry-based detection: if any
`origin/integration/*` branch tip becomes newly reachable from the pushed
commits, the push is blocked. There is no bypass variable.

The default branch is detected via `refs/remotes/origin/HEAD` (fallback:
`main`). The check catches `--no-ff`, `--ff-only`, default merges and rebases;
only cherry-picks (which produce new SHAs) are not detected. It matches only
branches under `integration/`.

**Requires**: `core.hooksPath` must be configured for the hook to be active.
New rigs get this automatically. Existing rigs: run `gt doctor fix git-exclude-configured`.

## Anti-Patterns

### Creating the Branch After Work Starts

Children slung before the integration branch exists spawn from main and land
on main. Create the branch first.

### Manually Targeting the Integration Branch

Auto-detection handles targeting. If a polecat did not pick the branch up,
check that the branch exists on origin and that the issue is a descendant of
the epic.

## See Also

- [Polecat Lifecycle](polecat-lifecycle.md) — How polecats submit work for landing
- [ADR 0004](../adr/0004-daemon-lands-work.md) — The landing worker
