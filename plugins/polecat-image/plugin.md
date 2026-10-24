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
nothing else removes the old tags (gt-mexpf). At the end of every run (current,
deferred or built, never after a failure) the plugin untags the per-commit tags
beyond the newest 3 (`POLECAT_IMAGE_KEEP`) with a plain `docker rmi
<repo>:<sha>`. It never touches `:dev`, the image `:dev` names or a tag that is
not a hex commit, never passes `-f`, and never prunes anything else. A failed
removal keeps the tag and logs docker's own reason, `kept ... (rmi failed:
<reason>)`, so a container using the image and a real error read differently.
The build cache is not touched.

## Build cache

Each rebuild also leaves its layers in Docker's build cache (apt, the Go
toolchain, golangci-lint, the gt and bd compiles): a day of installs made it 20
GB with nothing in use (gt-atxdz). After the image prune the plugin runs
`docker builder prune --keep-storage 8GB -f` (`POLECAT_IMAGE_BUILDER_KEEP`):
LRU, so the most recently used 8 GB stays and the next rebuild is warm. It runs
that command and nothing else that deletes (never a system, image, volume or
`--all` prune; the per-rig `gt-cache-*` volumes are never touched), and it runs
under the plugin's lock: a run that built already owns it, any other takes it,
and a run that finds another's lock, or whose lock was taken during the build,
skips with a logged line, so a trim never overlaps a build. A down docker, a
failed build or a failing prune trims nothing and does not fail the run.

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
