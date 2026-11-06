# Cutover runbook: gt, bd, om and hm become the wrapper

Status: operator-gated, written 2026-11-06. Parent design:
[gt runs from the daemon image](gt-in-container.md), "Cutover and rollback".

Read the parent design before running this runbook: it says why the four
binaries are replaced and what the wrapper does.

## Pre-checks

Every line must hold before `gt-wrap install` runs. The install command prints
this list, and `TestCutoverRunbookNamesEveryPrecheck` holds the two equal.

- the dry run against every Mac caller passed
- the compose environment names the Dolt host, so bd in the container reaches Dolt
- the .beads mount is read-write in the daemon container
- docker-exec load was measured with the monitors running

The dry run is every Mac caller of the wrapper: the scripts under
`$HOME/.claude/tools`, `gt-watch`, the Claude hooks (`gt prime`) and
`claude-agent`.

## Cutover

Run install from the directory holding the new `gt-wrap`:

```bash
gt-wrap install --stamp 20261106-1430 \
  --target gt=/Users/operator/.local/bin/gt \
  --target bd=/Users/operator/.local/bin/bd \
  --target om=/Users/operator/go/bin/om \
  --target hm=/Users/operator/go/bin/hm
```

`--dir` names one directory holding all four instead. The command refuses
before changing anything when Docker, the daemon image or a tool in it does not
answer, when a file it would replace is not a regular file, or when a file
dated with the same stamp already exists. It replaces each tool by renaming the
old file to `<name>.pre-wrap-<stamp>` and renaming a new copy into place, so a
running binary is never written through. When a placed tool fails `--doctor`,
or a later step fails after a tool was placed, install undoes the tool that
failed, stops, and prints the rollback command and the dated files for the
tools it left installed.

The last lines install prints are the exact rollback command and the dated
files it kept, on success or on a stop. Keep them.

## The watch hour

Watch for one hour. The cutover holds when the overseer heartbeat, a landing and
a Claude session start each work through the wrapper.

## Rollback rule

A break during the watch hour that is not fixed within 15 minutes is rolled
back, not fixed forward. Run the `rollback:` command install printed:

```bash
gt-wrap rollback --stamp 20261106-1430 \
  --target gt=/Users/operator/.local/bin/gt \
  --target bd=/Users/operator/.local/bin/bd \
  --target om=/Users/operator/go/bin/om \
  --target hm=/Users/operator/go/bin/hm
```

Rollback renames each wrapper aside to `<name>.wrapper-<stamp>` and the dated
file back. A tool whose dated file is missing is refused, naming it, and left as
it is. Keep both sets of files until the quiet week in the parent design passes;
then delete them.
