+++
name = "polecat-image"
description = "Rebuild gastown-polecat:dev at the installed gt's commit after each gt install, so sandbox slings are not refused for image drift"
version = 1

[gate]
type = "cooldown"
duration = "10m"

[tracking]
labels = ["plugin:polecat-image", "category:sandbox"]
digest = true

[execution]
type = "script"
timeout = "15m"
+++

# Polecat Image

A sandbox polecat runs the **image's** gt (hooks, `gt done`, `bd` access)
against the live daemon and Dolt, so the image must carry the same gt as the
installed one. The sandbox backend refuses a sling whose image carries another
commit (gt-pmxo9); gt is reinstalled after most landings, so without this
plugin every sandbox sling would be refused until someone rebuilt the image by
hand.

`rebuild_gt` installs at most once per 30 minutes, so this plugin builds at most
about 48 times a day; a build is two cross-compiles and a `docker build`, about
a minute.

## What it does

1. Reads the installed gt's commit (`gt version`) and the commit label of
   `gastown-polecat:dev` (`org.gastown.gt-commit`). Equal (a prefix of at least
   7 characters either way): prints `current` and stops.
2. Defers (exit 0, a `deferred:` line, retried by the next cooldown) while Docker
   Desktop is stopped, while a `gt sling` process is running, or while the
   gateway lists a sandbox whose registry record under
   `~/gt/.runtime/sandboxes` does not exist yet, so a retag cannot land between
   a Prepare's image check and its create. A probe that cannot answer (`pgrep`
   or `openshell sandbox list` exiting with an error) defers too, never reads as
   "nothing running". The same check runs again before each build round.
3. Takes the lock `~/gt/.runtime/polecat-image.lock` (a lock older than 20
   minutes is taken over by renaming it aside, and only the run that wins the
   rename proceeds); a second run while it is held prints `already running` and
   stops. A run removes the lock only while its own pid is in it.
4. Makes a detached worktree of the rig repo at the installed commit under
   `~/gt/gastown/.polecat-image-build/<sha>`, runs `make polecat-image
   BD_SRC=~/gt/beads/mayor/rig` there, removes the worktree, and checks the
   label it ended with.
5. Reads the installed commit again; if a newer install landed during the
   build it builds again, at most 3 rounds.

The build's worktree is a detached one on the rig repo's shared `.repo.git`. No
cleanup path treats it as a stray: the reaper and `heldByOtherWorktree` only
`git worktree prune` registrations whose directory is gone (the directory exists
for the whole build), they match worktrees by branch name (a detached HEAD has
none), and the local-branch prune reads the list only to protect branches.

## Old images

Every build tags the image `gastown-polecat:<short sha>` as well as `:dev`, and
nothing else removes the old tags: a day of installs left dozens of 1.5 GB
images and a rising Docker disk (gt-mexpf). At the end of every run (current, deferred
or built, never after a failure) the plugin untags the per-commit tags beyond the
newest 3 (`POLECAT_IMAGE_KEEP`) with a plain `docker rmi <repo>:<sha>`. It never
touches `:dev`, the image `:dev` names or a tag that is not a hex commit, never
passes `-f`, and never prunes anything else: Docker refuses to remove an image a
container (a running sandbox) uses, and that refusal is logged as `kept ... (in
use)`, not an error. The build cache is not touched.

Every failure exits non-zero with the tail of the failing command: a commit the
rig repo does not know, a failed build, an image that still carries another
commit afterwards, an installed gt whose commit cannot be read. A deferral is not a
failure: it exits 0 and prints why, so a plugin whose receipts show `deferred:`
run after run is stuck behind that reason (Docker stopped, a sling that never
ends) and the image stays stale, with the sandbox sling's refusal as the net.

Run it directly:

```bash
bash ~/gt/plugins/polecat-image/run.sh
```

Its tests are `run_test.sh` beside it (stubbed gt, docker, git, make, openshell).
