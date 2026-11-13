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

`gt prime` runs from `~/gt`, as the session-start hook does. Through the wrapper it
refuses from a crew worktree, because the pod has no such directory and so cannot
run its worktree check or identity lock.

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
dated with the same stamp already exists. It stages a new copy beside every
tool before it touches any tool, so a failure while staging changes nothing. It
then replaces each tool by renaming the old file to `<name>.pre-wrap-<stamp>`
and renaming the staged copy into place, so a running binary is never written
through. When a tool fails `--doctor`, or a step fails after a tool was placed,
install puts every tool it changed back from its dated file, in reverse order,
stops, and prints one line per tool. A tool it cannot put back is named, with
the command that puts its original file back by hand and the rollback command
for it.

Before install, on the Mac, clear the immutable flag on gt and om. The old
installer set it (`chflags uchg`, gt-vya0s) so a stray write could not replace a
production binary; `ls -lO` shows `uchg`. A flagged file cannot be renamed, so
install would stop at its first target. Run `chflags nouchg <path>` on each
flagged tool as an approved operator step. The flag is not set again: the reason
for it, a local polecat that overwrote the binary, is gone, since polecats run in
sandboxes with no access to the Mac's files, and after the cutover the installed
files are the wrapper. Preflight cannot read the flag portably, so install does
not check for it. When the first rename of a tool is refused for permission,
install says the file may carry the flag, names the `chflags nouchg` command, and
says nothing was changed for that tool; every tool already placed is put back.

`--keep-old name=path` gives that tool's old file a second name, a hard link in
the same directory (so on one filesystem), made right after the old file is
renamed to its dated name. Use it for om:
`--keep-old om=/Users/operator/go/bin/om-native`, and set `native.om.path` to
that path once. It stays valid after install and after rollback, since it is the
same file as the dated old om and then the restored om, so the config never names
a stamp. Keep the old bd the same way: `--keep-old bd=/Users/operator/go/bin/bd-native`,
and set `native.bd.path` to that path with `"subcommands": ["hooks"]`. Only
`bd hooks run` then runs on the Mac, since the beads git hooks need the Mac's
files; the other bd commands reach the pod as before. Keep the old gt the same
way, `--keep-old gt=/Users/operator/go/bin/gt-native`, and set `native.gt.path`
to that path with `"subcommands": ["town forward plist", "maint plan", "maint apply"]`:
those calls print the launchd job's Mac log paths and read the Mac's Docker
contexts, which the pod lacks (gt-aea89k). Every other gt command reaches the pod
as before. A native path that is a tool's own installed path would run the
wrapper itself, and the wrapper refuses it. Install refuses a second name that
is any tool's path, dated name or aside name, or is asked for twice, before it
changes anything. Delete the second name and the dated files together after the
quiet week.

To replace the installed wrappers with a newer build, run `gt-wrap install` again
from the new gt-wrap with a new stamp and the same `--target` paths, and no
`--keep-old`: each wrapper is a regular file, so install replaces it the way it
replaces a tool above. Never `cp` over a wrapper: copying over a running signed
binary gets it killed. With
two stamps, `gt-wrap rollback --stamp <new stamp>` puts the previous wrapper back,
and `gt-wrap rollback --stamp <first stamp>` puts the native tools back from either
state (the wrapper at the path is moved aside as `<name>.wrapper-<first stamp>`).
Keep the dated files of both stamps until the quiet week ends.

On success the last lines install prints are the exact rollback command and the
dated files it kept. Keep them.

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
