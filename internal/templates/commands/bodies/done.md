# Done — Submit Work for Landing

Signal that your work is complete and submit your branch for landing.

Arguments: $ARGUMENTS

## Pre-flight Checks

Before running `gt done`, verify your work is ready:

```bash
git status                          # Must be clean (no uncommitted changes)
git log --oneline origin/main..HEAD # Must have at least 1 commit
```

If there are uncommitted changes, commit them first:
```bash
git add <files>
git commit -m "<type>: <description>"
```

## Execute

Run `gt done` with any provided arguments:

```bash
gt done $ARGUMENTS
```

**Common usage:**
- `gt done` — Submit completed work (default: --status COMPLETED)
- `gt done --target <branch>` — Submit against a branch other than the rig default
- `gt done --status ESCALATED` — Signal blocker, submit nothing
- `gt done --status DEFERRED` — Pause work, submit nothing

**If the bead has nothing to implement** (already fixed, can't reproduce):
```bash
bd close <issue-id> --reason="no-changes: <brief explanation>"
gt done
```

## While it runs

**`gt done` runs the local gate itself (lint, build and the unit tier of the tests; no container slot), which can take several minutes. That is normal. Do not interrupt it and do not close the bead.**

**Never script a retry around `gt done`.** The dangerous-command guard refuses
the loop shape. If it exits non-zero it names what failed (exit codes 10-16,
`gt done --help`) and your session stays up: fix it, commit, and run `gt done`
once more. If you believe a gate failure is not caused by your change, add a
bead comment with the error, then `gt escalate -s medium`, and wait.
No flag skips the gate.

Before you run `gt slot run`, read the container-gate rule in `docs/reference.md`.

This command rebases your branch, runs the local gate, pushes the branch, marks
the bead ready to land and exits the polecat session. The daemon's landing
worker gates the merged tree and lands it (ADR 0004).
