# Disk maintenance

The town's sandbox registry holds a ~2 GiB polecat image per commit, and
nothing deleted a tag from it until slice 1 of the disk-maintenance plan
(`docs/plans/2026-10-28-disk-maintenance-design.md`, historical) shipped a Go
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
| the `disk_maintenance` patrol | plans every target in a quiet daily window, and any target with an interval of its own on that interval; applies in `apply` mode |

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
`colima` (the sandbox data disk, through `colima ssh -p gt-sbx`), and
`docker-desktop` (the CI runner container's root filesystem, through
`docker exec`).

- At 80% used a disk raises `diskmaint:<disk>`; the alert clears only below
  70%, so a disk at the line does not flap.
- A disk whose read fails or whose output it cannot parse raises
  `diskmaint:<disk>:unreadable` and clears when a later read works: a disk that
  cannot be measured is not a quiet disk.
- Alerts fire in `dry_run` mode too. They are raised before the plan, so a run
  that goes on to refuse still sounds the alarm.

While any readable disk is at or above 80%, the pass keeps 3 of the newest
candidate tags instead of the configured keep; a keep already at or below 3
stays where it is. A disk that could not be read refuses that day's pass
outright — a keep decided without it would be a guess — and the window counts
as settled, so the next pass is the next day's. A deferred apply is the one
conclusion that leaves the window open, because the push holding the lock is
what to wait for.

## The shared cache claim

The patrol also trims the shared cache claim a kubernetes sandbox profile
mounts, once per window and behind the same mode:

- The claim's listing is read through a running pod that mounts it, with the
  polecat image's `trim-list` helper. With no such pod there is no listing, and
  the pass logs that it trims nothing rather than guessing one.
- `dry_run` logs the Job and its shard ConfigMaps; `apply` deletes what an
  earlier run left behind, applies the shards before the Job that mounts them,
  and deletes the shards once the Job has finished. A Job still running when
  the wait ends keeps its shards for the next run.
- The cap is `POLECAT_CACHE_CAP`, the knob in the
  [configuration reference](../reference.md#disk-maintenance), read here for the
  shared claim and by the plugin for each per-rig volume.
- The pass trims nothing when two profiles mount one claim with different
  images, or when two claims are mounted in one namespace: a trim's labels
  cannot tell one claim's objects from the other's.

## Rollback

A wrong delete costs a rebuild, not data: any per-commit image is rebuilt from
its commit (`make polecat-image` in a worktree of that commit) and republished
by the next plugin run. To stop pruning at once, turn the plugin's prune switch
off and disable the patrol. The ledger names every manifest digest deleted and
when.
