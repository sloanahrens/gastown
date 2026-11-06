+++
name = "polecat-image"
description = "Rebuild gastown-polecat:dev at the installed gt's and bd's commits; publish it to the configured registry; then trim the image tags, build cache and per-rig cache volumes a run grows"
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

A sandbox polecat runs the **image's** gt (hooks, `gt done`, `bd` access), so
the image must carry the installed gt's commit. The sandbox backend refuses a sling whose image carries another
commit (gt-pmxo9); gt is reinstalled after most landings, so without this
plugin every sandbox sling would be refused until someone rebuilt the image by
hand.

`rebuild_gt` installs at most once per 30 minutes, so this plugin builds about
48 times a day; a build is three cross-compiles and a `docker build`, about a
minute.

`make install` (`cmd/gt-installer`) runs this plugin itself: by default it
builds the image before it swaps the binary and fails the install if the build
does not finish, and `--no-image-first` swaps first and builds the image
detached, escalating a failure under `install-gt:image-build-failed`.

## What it does

1. Reads the installed gt's and bd's commits (`gt version`, `bd version
   --json`) and the image's `org.gastown.gt-commit` and `org.gastown.bd-commit`
   labels. Each pair equal (7 characters as a prefix either way): prints
   `current` and stops, so a bd-only landing still rebuilds (gt-xudej). An
   unreadable bd leaves the bd half current and logs one line.
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
   `~/gt/gastown/.polecat-image-build/<sha>`, runs `make polecat-image` there,
   removes the worktree, and checks the labels it ended with. The recipe first
   runs `internal/polecat/imagectx/cmd/build`, which stages the build context
   from the pins: the bd source in `deploy/daemon/image-pins`, each seeded rig
   read from `deploy/polecat/seed-pins` and fetched from its URL in
   `deploy/polecat/seed-urls`, and gt and the plugin binaries from the worktree
   at the installed commit. No bd or rig checkout under the town is read. The
   recipe's docker build follows it, taking the gt commit, the bd commit and the
   Go floor the command prints, and the run publishes the image as before
   (gt-gg9hv.33).
5. Reads both commits again; if either moved it builds again, at most 3 rounds.
   A bd label still behind then fails the run, naming both commits, and records
   the pair in `~/gt/.runtime/polecat-image.bd-stuck`. A later run that finds gt
   current and the same pair fails at once without building, until either
   commit moves; a run that finds the image current clears the record.
6. With a registry resolved ("Publishing to a registry"), publishes what it just
   built there, and retries a publish that did not complete for an image that is
   already current.

Before adding a cleanup path that could touch the build's worktree, read `docs/polecat-image-notes.md`.

## Publishing to a registry

A Kubernetes seat pulls the image from a registry, not from the Mac's Docker
daemon (`docs/design/k8s-sandboxes.md`). `POLECAT_IMAGE_REGISTRY` in the
environment (a `host:port` such as `localhost:5001`) wins; unset, the plugin
reads it and `POLECAT_IMAGE_CRANE` from daemon.env in the town's
`settings/config.json`, no other key, since a hand install's shell leaves both
unset (gt-du3fi). Naming neither publishes nothing; an unreadable config fails
the run.

- **Name change.** `POLECAT_IMAGE_REGISTRY` once named the gateway's
  sandbox-records directory; that is `POLECAT_IMAGE_SANDBOX_RECORDS` now. A
  `POLECAT_IMAGE_REGISTRY` that is a path is refused with a log line naming the
  new variable.
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

## Registry retention

After the trims, a run holding the lock prunes the sandbox registry.
`POLECAT_IMAGE_REGISTRY_KEEP` is the newest commit tags kept (default `5`);
`POLECAT_IMAGE_REGISTRY_PRUNE` is `off`, `dry-run` (the default) or `apply`. No
outcome fails the run. The rules are in the `BuildPlan` comment in
`internal/diskmaint/registry/plan.go`.

## Old images

Every build tags the image `gastown-polecat:<short sha>` and `:dev`, and nothing
else removes old tags (gt-mexpf). At every run's end (current, deferred or
built, never a failure) the plugin untags per-commit tags beyond the newest 3
(`POLECAT_IMAGE_KEEP`) with a plain `docker rmi <repo>:<sha>`: never `:dev`, a
non-hex tag or `-f`. A failed removal keeps the tag and logs docker's reason
(`kept ... (rmi failed: <reason>)`), so an in-use image and a real error read
differently. With a registry, the `<registry>/…:<sha>` alias is untagged first
(it would keep the image on disk); a refused alias keeps the tag. The build
cache is untouched. A `POLECAT_IMAGE_KEEP` that is not a whole number, or an
unreadable listing, keeps every tag and logs why; the run continues.

`POLECAT_IMAGE_TRIM_CONTEXT`, which the daemon sets from
`tier_sweep.runner.context` for docker and kubernetes, repeats the trim with
`docker --context <name>`, keeping any tag a container there uses and naming
the context in each line.

## Build cache

Each rebuild also leaves its layers in Docker's build cache (apt, the Go
toolchain, golangci-lint, the gt and bd compiles), which grew to 20 GB unused
(gt-atxdz). After the old-image trim it runs
`docker builder prune --keep-storage 8GB -f` (`POLECAT_IMAGE_BUILDER_KEEP`):
LRU, so recently used layers stay and the next rebuild is warm. Then it runs
`docker image prune -f` (dangling images only, gt-n0tfr). No other prune runs:
never a system, volume, `--all` or `-a` prune. Both run under the plugin's lock:
a run that built already owns it, any other takes it, and a run that finds
another's lock, or whose lock was taken mid-build, skips with a logged line, so
no trim overlaps a build. A down docker, a failed build or a failing prune trims
nothing and does not fail the run.

## Per-rig cache volumes

Every sandbox of a rig mounts that rig's `gt-cache-<rig>` volume at
`/sandbox/.cache`, and nothing trimmed it (gt-3g7p2). After the build cache trim
the plugin brings each `gt-cache-<rig>` volume back under `POLECAT_CACHE_CAP`
(default 10GB; a K, M, G, T or P suffix, so `512M` works):

A volume with a container attached is skipped (`in use by N container(s);
skipped`). A probe the plugin cannot read trims nothing and logs which one, so a
failure never reads as "nothing to do".

Over the cap, a short-lived container of the polecat image runs
`bin/polecat-image trim-cache` and deletes files under `go-build`, least recently
used first, until the volume fits. The trim runs in the image, since a docker
volume has no host path. "Used" is the later of a
file's atime and mtime, because Go touches the mtime of a cache entry it reuses
and a noatime mount leaves atime stale. `go/pkg/mod` goes only when the build
cache alone cannot reach the cap. The helper follows no symlink, so it never
removes a path outside the volume. No volume is removed and no `docker volume
prune` runs.

A sandbox starting between the in-use check and the helper can lose a cache
entry; Go rebuilds the miss, so the window is accepted, not locked.

Every failure exits non-zero with the tail of the failing command: a commit the
rig repo does not know, a failed build, an image still carrying another commit,
an installed gt whose commit cannot be read. A deferral exits 0 and prints why,
unless `POLECAT_IMAGE_FAIL_CLOSED=1` makes it exit non-zero; read "Publishing to
a registry" before setting that variable. A plugin whose receipts show
`deferred:` run after run is stuck behind that reason, with the sandbox sling's
refusal as the net.

Run it directly; before building or syncing this plugin, read "Script entry
point" in `plugins/README.md`:

```bash
~/gt/plugins/polecat-image/bin/polecat-image
```

Tests: `go test ./internal/polecatimage/`.
