# Disk maintenance

The town's sandbox registry holds a ~2 GiB polecat image per commit, and
nothing deleted a tag from it until the disk-maintenance work shipped a Go
pruner for it. `internal/diskmaint` measures, plans and applies the deletions,
writes a ledger line per apply, and refuses a plan it cannot justify from what
it read.

Read this before turning any of the switches to `apply`, or before reading a
ledger line to find out why an image went.

## The three entry points

All three run the same `registry` target and share the plugin's lock.

| Entry point | What it does |
|---|---|
| the `polecat-image` plugin | plans after its own image, cache and volume trims, and applies in `apply` mode, in the run that holds the lock |
| `gt maint` | plans by hand, or applies the plan it just printed because it was typed; a target name it does not know is listed back |
| the `disk_maintenance` patrol | plans every target in a quiet daily window, and any target with an interval of its own on that interval; trims the shared cache claim on its own interval; applies the disk alerts on busy ticks through the alert-only pass; applies in `apply` mode |

`registry` is the only target in slice 1. `plan` is the dry run, so it creates
neither the ledger directory nor anything else; both `plan` and `apply` exit
non-zero on a target that refused or failed.

Before turning any of the three on, read [the configuration reference](../reference.md#disk-maintenance):
it has each switch, what it accepts, which entry point reads it, and the
default each starts in. `gt maint --help` states its own modes and arguments.

## What a prune deletes, and what it never deletes

Only tags of `gastown-polecat` shaped like a short commit hash: lowercase
hexadecimal, 7 to 12 characters. Every other tag is a name, and a name is never
a candidate — `dev` among them.

The registry deletes a manifest by digest, and that drops every tag pointing at
that digest. So the plan works on digests: it builds the set it keeps and
deletes only the digests outside it. Everything in this set is never deleted:

- the digest `dev` points at;
- the digest of the tag that is a prefix of the installed gt commit, read from
  `gt version`;
- every digest a running sandbox pod or a sandbox record names, by image ID and
  by the tag the container or record spells out;
- the newest candidate tags by manifest creation time, up to the configured
  keep.

A candidate tag that shares its digest with anything kept survives with it:
deleting the digest would delete the kept tag too. Two tags on one digest are
one delete.

Two stray repositories, `gastown-polecat-measure` and `seedtest/polecat`, are
deleted whole, and only when nothing running or recorded names them.

After at least one delete succeeds, garbage collect runs inside the registry
container to reclaim the blobs, unless the registry reports a push in flight;
a skipped collect leaves the space to the next apply, and the ledger says so.

## When a run refuses

A plan that could not read one of its inputs is empty on purpose with
`Refused` set, never a guess:

- the catalog, a tag listing, or a manifest could not be read, or a name in
  them fails validation;
- `gastown-polecat` has no `dev` tag, so there is nothing to anchor the keep
  set on;
- the installed commit is unknown (`gt version` named none);
- the pod listing failed, or the sandbox records directory is missing — a town
  that runs sandboxes has one.

An apply also stops when the registry answers a delete with "deletes
disabled": the container was recreated without
`REGISTRY_STORAGE_DELETE_ENABLED=true`, and the remaining actions are counted
rather than attempted. The plugin logs a refusal and fails no run with it; the
patrol logs one; `gt maint` prints it and exits non-zero.

## The lock

Every apply runs inside the `polecat-image` lock
(`<town>/.runtime/polecat-image.lock`) that every push holds, so a delete never
races an image the plugin is pushing.
The plugin already holds the lock when it prunes; `gt maint apply` and the
patrol's apply take it, and an apply that finds it held deletes nothing and
reports a deferral. A dry run takes no lock.

## The ledger

Every apply appends one JSON line to the maintenance ledger,
`diskmaint/ledger.jsonl` under the town's runtime directory. When a line would
push the live file past a mebibyte it rotates to `.1` first, discarding an older
rotation. A `plan` writes nothing, so the ledger is the record of what was
applied.

```json
{"ts":"2026-10-29T04:31:07Z","target":"registry","mode":"apply","planned":4,"done":4,"bytes_freed":8589934592}
```

A line names the target, the mode, how many actions were planned and done, the
bytes freed, and any refusal, deferral or error, including why garbage collect
was skipped. The last line of the live ledger after a run is the quickest check
that it did what the plan said.

## Disk alerts and keep tightening

The patrol reads three disks: `mac` (the machine's root filesystem, `df -P /`),
`colima` (the sandbox data disk, the worst node's used share from the kubelet
node stats, so the read works inside the town's container where there is no
colima binary), and `docker-desktop` (the root filesystem of the `gt-dolt`
container, read through `docker exec`).

When `patrols.tier_sweep.runner` names a kubernetes context, the patrol reads a
fourth, `control`: the control cluster's node disk (`/var/lib/rancher`), read the
way `colima` is, through the same kubeconfig and that context. With no context
the disk is not read and the patrol logs one line, since the town may run without
a control cluster.

- At 80% used a disk raises `diskmaint:<disk>`; the alert clears only below
  70%, so a disk at the line does not flap.
- A disk whose read fails or whose output it cannot parse raises
  `diskmaint:<disk>:unreadable` and clears when a later read works: a disk that
  cannot be measured is not a quiet disk.
- Alerts fire in `dry_run` mode too. They are raised before the plan, so a run
  that goes on to refuse still sounds the alarm.
- `control` is alert-only. Its alerts raise and clear like the others, but an
  unreadable or full `control` disk never tightens the keep and never refuses a
  plan: the keep and the plans are decided by the town's own three disks alone.
- A `control` raise names the three biggest consumers on the disk, from the same
  kubelet summary: per pod, the larger of its ephemeral-storage usage and the sum
  of its claim-backed volumes, as `namespace/pod 61 GB`. Cluster names are cut to
  200 characters and stripped of line breaks before they reach the alert.
- Kubelet eviction starts at about 85% to 90% used on this node (the 91% of
  2026-11-05), so the 80% alert leaves five to ten points. The alert-only pass
  (below) keeps that margin watched while the town stays busy.
- The disk name is the middle part of the alert key, `diskmaint:<disk>`. Renaming
  a disk orphans any open alert under the old name.

While any readable keep disk (`mac`, `colima`, `docker-desktop`) is at or above
80%, the pass keeps 3 of the newest candidate tags instead of the configured
keep; a keep already at or below 3 stays where it is. A keep disk that could not
be read refuses that day's pass
outright — a keep decided without it would be a guess — and the window counts
as settled, so the next pass is the next day's. A deferred apply is the one
conclusion that leaves the window open, because the push holding the lock is
what to wait for.

### The alert-only pass

A busy tick whose trim is not due runs the alert-only pass, whether or not a
target is due: it reads the disks and applies their alerts through the same
application a trim or a full pass uses, and it does nothing else. A target the
tick found still waits for a quiet tick. Its spacing is ten minutes
(`diskMaintBusyAlertEvery`), measured from the last application any pass made,
so a trim or a full pass that applies the alerts restarts it, and the pass never
repeats a reading another pass just took. Its own fallback read, the one it
makes when another pass holds the turn, spends that spacing too, so a turn held
by a slow notifier does not put it back on the disks every tick. It has a
single-flight guard of its own, and it waits for the alert turn like any other
application: when the wait runs out it reads the disks alone, leaves the alerts
to the pass that holds the turn, and says so at most once an hour. It writes
nothing to the schedule, so it can neither spend the trim's interval nor delay
a target.

## The shared cache claim

The patrol also trims the shared cache claim a kubernetes sandbox profile
mounts, on its own interval (`cache_trim_interval`) in any window, and behind
the same mode. The trim runs on a pass of its own and does not wait for the town
to be quiet: it applies ConfigMaps and a Job on the sandbox cluster and reads
neither the landing worker nor Dolt, so a landing pass in flight cannot starve
it while the sandbox disk fills. That pass reads the disks and applies their
alerts beside the trim, so a notifier that does not answer delays the end of
the alerts and not the trim. Alert applications from different passes take
turns, so a later reading is never applied over an earlier one's state, and
every other target still waits for a quiet tick. A notifier that does not
answer holds the alert turn for up to three attempts of the 60-second escalation
timeout for each alert; another pass waits 30 seconds for the turn, then reads
the disks for its own decisions and goes on, and the alerts of that time stay
with the holder. A tick that finds no trim due runs no trim: a busy one applies
the alerts through the alert-only pass (above).

- A claim's namespace comes from the claim itself, and every pod read, listing
  exec and apply, delete or wait names it. A claim of the same name in another
  namespace is another claim, read and counted from its own namespace's pods.
- The claim's listing is read through a running pod that mounts it, with the
  polecat image's `trim-list` helper. Every pod that mounts the claim and has
  not finished counts as a sandbox; with no running one there is no listing,
  and the pass logs that it trims nothing rather than guessing one.
- A removal list past the shard ceiling is carried in the claim itself, by one
  Job whose init container plans the list and writes it there. The applied line
  names the route: `route=claim`, or `route=shards n=<count>` for the ConfigMap
  route. For that route the pass reads the claim's free space with `df -Pk` in
  the same pod the listing came from. A list larger than that room is refused
  before anything is applied, counts as a failed run, and is retried fifteen
  minutes later. A `df` that cannot answer leaves the room unknown: the trim
  still runs, and the pass logs `free space unknown: <reason>` at most once an
  hour.
- `dry_run` logs the Job and its shard ConfigMaps; `apply` deletes what an
  earlier run left behind, applies the shards before the Job that mounts them,
  and deletes the shards once the Job has finished. An earlier Job that is
  still running is left to finish, and the pass applies nothing that window.
  A Job still running when the wait ends keeps its shards for the next run.
- The cap is `POLECAT_CACHE_CAP`, the knob in the
  [configuration reference](../reference.md#disk-maintenance), read here for the
  shared claim and by the plugin for each per-rig volume.
- The pass trims nothing when two profiles mount one claim with different
  images, or when two claims are mounted in one namespace: a trim's labels
  cannot tell one claim's objects from the other's.

Between those runs the trim also runs early. On a five-minute tick that finds
the trim not due, the patrol reads the sandbox disk, and when it is at or over
`cache_trim_early_percent` it runs the trim now, once the gap since the last
trim's start has passed; the default, the range and the gap are in the
[configuration reference](../reference.md#disk-maintenance). An early run does
not start while a trim holds its guard or while a failed run's retry is pending.
Its one log line is `disk_maintenance: cache trim early: sandbox disk <n>% >=
<m>%`. A disk reading that fails runs no trim; the patrol logs
`disk_maintenance: cache trim early: sandbox disk read failed: <error>` once per
gap, and the alerts raise the failure.

## The CI runner's Go build cache

The patrol also trims the Go build cache the CI gate jobs fill on the control
cluster. The cache is the named volume `ci-gocache`, under `/var/lib/docker` in
the `dind` container of the `forgejo-runner` deployment in the `forgejo`
namespace. The trim runs on its own interval (`ci_cache_trim_interval`) in any
window and does not wait for the town to be quiet, as the shared cache claim's
trim does, so a landing pass in flight does not hold it off.

- The three settings sit under `patrols.disk_maintenance`: `ci_cache_trim_interval`
  (default `1h`), `ci_cache_max_age` (default `12h`) and `ci_cache_warn_bytes`
  (default `30GB`). An empty setting is the default. Zero or less is refused, and
  the error names the setting.
- The exec goes to the control cluster, which is the context
  `patrols.tier_sweep.runner.context` names. The runner must be on the kubernetes
  backend with that context set. On any other backend, or with no tier sweep
  runner at all, the trim is not configured: it runs nothing and logs nothing. A
  kubernetes runner whose context is empty, or leads with a dash, runs nothing
  and says why.
- Each run is one exec into the `dind` container. It removes the files under the
  volume's data directory whose last write is older than `ci_cache_max_age`, then
  every empty directory below the top level, including any that was already
  empty before the run. The volume's top level is left to Go: its `trim.txt`
  bookkeeping and its 256 two-hex-digit shard directories, which Go creates once
  and does not recreate. `ci-gomod` and `ci-npm` are not touched.
- The mode applies as it does to the rest of the patrol. `dry_run` lists what
  would be removed and removes nothing; `apply` removes. An applied run logs one
  line, `disk_maintenance: ci cache trim: removed N files, X to Y (older than 12h)`,
  naming the sizes before and after and the max age.
- A run that cannot tell what the volume holds is a failed run, never a volume
  with nothing to trim. A missing volume path, a `dind` container that is not
  running, a failed exec and an unreadable reply each log their own line, at most
  once an hour, and the trim is retried in 15 minutes.
- An applied trim that removes no file while the volume is over
  `ci_cache_warn_bytes` raises one low attention item, keyed `ci-cache-trim-noop`.
  The item means the trim is not reaching what is there, or that nothing is yet
  older than the max age. A later reading under the figure clears it.

## The CI runner's engine

The `dind` container's Docker data root holds more than the Go cache: images,
stopped containers, build cache and anonymous volumes, on a claim that nothing
bounds. The engine prune keeps the rest of the engine in check on its own
interval (`ci_engine_prune_interval`), under the same patrol, the same exec into
the `dind` container and the same `dry_run` default as the cache trim above.

- The settings sit under `patrols.disk_maintenance`: `ci_engine_prune_interval`
  (default `1h`), `ci_engine_prune_age` (default `48h`) and `ci_engine_keep_repos`
  (default `["ci-base"]`). An empty setting is the default. A zero or negative
  interval or age is refused, and an explicit empty keep list is refused, since it
  would leave the base images unprotected. A keep name that is not a repository
  name is refused. Each error names its setting.
- Each run is two execs. The first lists the engine as JSON and changes nothing.
  The planner decides what goes from that listing alone. The second removes only
  the ids and names the planner named, and is skipped in `dry_run`.
- What goes:
  - stopped containers (`created`, `exited`, `dead`) created more than 24 hours
    ago;
  - images no container uses that were created more than `ci_engine_prune_age`
    ago and carry no tag in a `ci_engine_keep_repos` repository other than
    `ci-base`;
  - `ci-base` images other than the one `ci-base:latest` names, and the id created
    just before it, which is kept as the rollback. These go without an age gate.
    A `ci-base` image that also carries a tag in another kept repository is kept;
  - anonymous volumes, whose names are the 32 hex digits docker gives one, with no
    `<name>-env` volume beside them, no container that stays mounting them, and
    created more than 24 hours ago;
  - build cache unused for 24 hours, pruned by docker itself in an apply.
- What stays: any image a container that stays uses; any image that is a parent
  of an image that stays, which docker would refuse to remove; named volumes, so
  `ci-gocache`, `ci-gomod` and `ci-npm` are never candidates.
- A dry run logs one line per container, image and volume it would remove, and a
  summary. It does not size the build cache, since docker has no dry run for it.
- An applied run logs `disk_maintenance: ci engine prune: removed N containers, N
  images, N volumes, X to Y`. The sizes are the data root's, read with `du`.
- A run that cannot tell what the engine holds is a failed run and removes nothing:
  a runner pod whose `dind` container is not running, a listing that does not
  decode, an id or time in an unexpected shape, a failed exec, or a remove reply
  whose count differs from the plan. Each logs its own line, at most once an hour,
  and the run is retried in 15 minutes. A removal docker refuses, for example a
  resource a new container has just started using, stops the apply there; the next
  run plans again.

## Rollback

A wrong delete costs a rebuild, not data: any per-commit image is rebuilt from
its commit (`make polecat-image` in a worktree of that commit) and republished
by the next plugin run. To stop pruning at once, turn the plugin's prune switch
off and disable the patrol. The ledger names every manifest digest deleted and
when.
