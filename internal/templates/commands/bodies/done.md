# Done — Submit Work to Merge Queue

Signal that your work is complete and ready for the merge queue.

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
- `gt done --pre-verified` — **(refinery/mayor only; polecats: do NOT use)**
- `gt done --status ESCALATED` — Signal blocker, skip MR
- `gt done --status DEFERRED` — Pause work, skip MR

**If the bead has nothing to implement** (already fixed, can't reproduce):
```bash
bd close <issue-id> --reason="no-changes: <brief explanation>"
gt done
```

## While it runs

**`gt done` waits for the container-gate slot before it runs the container suites, printing a `still waiting for the container-gate slot …` line every couple of minutes while it does. That is normal. Do not interrupt it, do not close the bead, do not retry. It gives up with a slot-acquire timeout once the cap expires.**

**Never poll the slot, and never script a retry around `gt done`.** A polling
loop holds the container-gate slot every other agent is queued behind, one pass
at a time, and the dangerous-command guard refuses the loop shape. If it fails
on the test-verify slot cap or the run budget: do NOT retry; add a bead comment
with the error and the verify-log path, then `gt escalate -s medium` asking the
mayor for a one-shot `--skip-verify` ruling, and wait. `--pre-verified` is not
that path: it is refinery/mayor only and re-runs the whole gate set under the
same slot cap.

Before you run `gt slot`, read the container-gate rule in `docs/reference.md`.

This command pushes your branch, submits an MR to the merge queue, and exits the
polecat session after durable handoff. The Refinery/Witness handle merge and cleanup.
