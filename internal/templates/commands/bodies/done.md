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

**gt done may sit silently for up to 20-30 minutes waiting for the container-gate slot. That is normal. Do not interrupt it, do not close the bead, do not retry. It will print a slot-acquire timeout if it gives up.**

Between its progress lines the pane is quiet, and a held gate is the ordinary
reason: `gt done` prints `still waiting for the container-gate slot …` every
couple of minutes while it waits, and the wait is capped by the rig's
`merge_queue.test_verify_slot_timeout` (60m by default).

**Never poll the slot, and never script a retry around `gt done`.** No
`for`/`while`/`until` loop, no `while true`, no watcher, no generated retry
script that re-runs `gt done` or reads `gt slot status`. A polling loop holds
the container-gate slot that every other agent is queued behind, one pass at a
time, while its own result is a coin flip — that is the incident this rule
exists for (gt-7dxw), and the dangerous-command guard now refuses the loop
before it starts. Run it once.

**If it fails on the test-verify slot cap or the run budget:** do NOT retry,
poll, or script. Add a bead comment with the error and the verify-log path, then
`gt escalate -s medium` asking the mayor for a one-shot `--skip-verify` ruling,
and wait (gt-pnkd). `--pre-verified` is not that path: it is refinery/mayor only
and re-runs the whole gate set under the same slot cap.

This command pushes your branch, submits an MR to the merge queue, and exits the
polecat session after durable handoff. The Refinery/Witness handle merge and cleanup.
