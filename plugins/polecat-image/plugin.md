+++
name = "polecat-image"
description = "Rebuild gastown-polecat:dev at the installed gt's commit after each gt install, so sandbox slings are not refused for image drift; publish it to the configured registry; then trim the image tags, build cache and per-rig cache volumes that run grows"
version = 1

[gate]
type = "cooldown"
duration = "10m"

[tracking]
labels = ["plugin:polecat-image", "category:sandbox"]
digest = true

[execution]
type = "script"
binary = "bin/polecat-image"
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
about 48 times a day; a build is three cross-compiles and a `docker build`, about
a minute.

`make install` (`cmd/gt-installer`) runs this plugin itself: by default it
builds the image before it swaps the binary and fails the install if the build
does not finish, and `--no-image-first` swaps first and builds the image
detached, escalating a failure under `install-gt:image-build-failed`.

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
   `POLECAT_IMAGE_FAIL_CLOSED` makes a deferral fail; "Publishing to a registry"
   has why.
3. Takes the lock `~/gt/.runtime/polecat-image.lock` (a lock older than 20
   minutes is taken over by renaming it aside, and only the run that wins the
   rename proceeds); a second run while it is held prints `already running` and
   stops. A run removes the lock only while its own pid is in it.
4. Makes a detached worktree of the rig repo at the installed commit under
   `~/gt/gastown/.polecat-image-build/<sha>`, runs `make polecat-image
   BD_SRC=~/gt/beads/mayor/rig` there, removes the worktree, and checks the
   labels it ended with; the Makefile's `BD_COMMIT` comment says which bd
   commit goes into the image and why (gt-7b7a5).
5. Reads the installed commit again; if a newer install landed during the
   build it builds again, at most 3 rounds.
6. With a registry resolved ("Publishing to a registry"), publishes what it just
   built there, and retries a publish that did not complete for an image that is
   already current.

The build's worktree is a detached one on the rig repo's shared `.repo.git`,
which no cleanup path treats as a stray: the reaper prunes only registrations
whose directory is gone and matches worktrees by branch name.

## Publishing to a registry

A Kubernetes seat pulls the image from a registry, not from the Mac's Docker
daemon (`docs/design/k8s-sandboxes.md`). `POLECAT_IMAGE_REGISTRY` in the
environment (a `host:port` such as `localhost:5001`) wins; unset, the plugin
reads it and `POLECAT_IMAGE_CRANE` from daemon.env in the town's
`settings/config.json`, no other key, since a hand install's shell leaves both
unset (gt-du3fi). Naming neither publishes nothing; an unreadable config fails
the run. A `POLECAT_IMAGE_REGISTRY` that is a path is refused with a log line:
the sandbox-records directory that name once meant is
`POLECAT_IMAGE_SANDBOX_RECORDS`.

- **Why `crane`.** Docker Desktop's list of insecure registries belongs to the
  overseer, so the plugin pushes with `crane` from a `docker save` tarball and
  tags the image locally with the registry reference, which is what the sandbox
  backend's drift check inspects. The image is saved and pushed once; its other
  tags are made on the registry with `crane tag`, which copies no layer.
- **A failed publish never breaks the plugin.** A missing `crane` or a failed list, tag,
  save or push is logged with its reason and the run exits 0, unless
  `POLECAT_IMAGE_FAIL_CLOSED=1` (below); nothing is removed. A push that did not
  complete is retried by the next run even when the image is already current.
- **Failing closed where it matters.** `POLECAT_IMAGE_FAIL_CLOSED=1` makes a
  run that leaves the image unbuilt or unpublished exit non-zero: every publish
  failure above, and every deferral (docker down, a sling running, the lock
  held). The installer sets it for the build that runs before the binary
  swap, so an install cannot swap a binary whose image is not in place
  (gt-efupr). Unset, as the daemon's own runs leave it, every path above keeps
  its exit 0.

## Old images

Every build tags the image `gastown-polecat:<short sha>` as well as `:dev`, and
nothing else removes the old tags (gt-mexpf). At the end of every run (current,
deferred or built, never after a failure) the plugin untags the per-commit tags
beyond the newest 3 (`POLECAT_IMAGE_KEEP`) with a plain `docker rmi
<repo>:<sha>`. It never touches `:dev`, the image `:dev` names or a tag that is
not a hex commit, never passes `-f`, and never prunes anything else. A failed
removal keeps the tag and logs docker's own reason, `kept ... (rmi failed:
<reason>)`, so a container using the image and a real error read differently.
With a registry set, the tag's `<registry>/…:<sha>` alias is untagged first (it
would keep the image on disk); an alias docker refuses to untag keeps the tag. A
`POLECAT_IMAGE_KEEP` that is not a whole number, or a tag listing docker cannot
answer, keeps every tag and logs why; the rest of the run carries on.

## Build cache

Each rebuild also leaves its layers in Docker's build cache (apt, the Go
toolchain, golangci-lint, the gt and bd compiles): a day of installs made it 20
GB with nothing in use (gt-atxdz). After the image prune the plugin runs
`docker builder prune --keep-storage 8GB -f` (`POLECAT_IMAGE_BUILDER_KEEP`):
LRU, so the most recently used layers stay and the next rebuild is warm. It runs
that command and no wider prune (never a system, image, volume or `--all`
prune), and it runs under the plugin's lock: a run that built already owns it,
any other takes it, and a run that finds another's lock, or whose lock was taken
during the build, skips with a logged line, so a trim never overlaps a build. A
down docker, a failed build or a failing prune trims nothing and does not fail
the run.

## Per-rig cache volumes

Every sandbox of a rig mounts that rig's `gt-cache-<rig>` volume at
`/sandbox/.cache`, and nothing trimmed it: `gt-cache-gastown` reached 27.7 GB on
2026-10-24, growing about 3 GB an hour (gt-3g7p2). After the build cache trim
the plugin brings each `gt-cache-<rig>` volume back under `POLECAT_CACHE_CAP`
(default 10GB; a K, M, G, T or P suffix, so `512M` works):

A volume a container has attached is skipped, and a probe the plugin cannot read
trims nothing and logs which one.

Over the cap, a short-lived container of the polecat image runs
`bin/polecat-image trim-cache` and deletes files under `go-build`, least recently
used first, until the volume fits. The trim runs in the image because a docker
volume has no host path: the volume is its only mount. "Used" is the later of a
file's atime and mtime, because Go touches the mtime of a cache entry it reuses
and a noatime mount leaves atime stale. `go/pkg/mod` goes only when the build
cache alone cannot reach the cap. The helper follows no symlink, so it never
removes a path outside the volume. No volume is removed and no `docker volume
prune` runs.

A sandbox starting between the in-use check and the helper can lose a cache
entry it would have read; Go reads a miss and rebuilds it, so the window is
accepted rather than locked.

## Registry retention

After the cache trim, a run holding the lock deletes the registry manifests
`docs/plans/2026-10-28-disk-maintenance-design.md` ("The registry target") puts
outside the keep set — `dev`, the installed gt's tag, the newest
`POLECAT_IMAGE_REGISTRY_KEEP` commits (default 5) and every digest a running
sandbox uses — then garbage collects. `POLECAT_IMAGE_REGISTRY_PRUNE` is `off`,
`dry-run` (the default) or `apply`; a read it cannot make, a count that is not a
number and any other mode delete nothing and log why, so the run still exits 0.
Applies are recorded in `~/gt/.runtime/diskmaint/ledger.jsonl`.

Every failure exits non-zero with the tail of the failing command. A deferral
exits 0 and prints why, unless `POLECAT_IMAGE_FAIL_CLOSED=1` ("Publishing to a
registry" has why), and a plugin whose receipts show `deferred:` run after run
is stuck behind that reason, with the sandbox sling's refusal as the net.

Run it directly; before building or syncing this plugin, read "Script entry
point" in `plugins/README.md`:

```bash
~/gt/plugins/polecat-image/bin/polecat-image
```

Tests: `go test ./internal/polecatimage/`.
