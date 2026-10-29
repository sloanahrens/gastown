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

| Entry point | What it does | Switch |
|---|---|---|
| the `polecat-image` plugin | plans after its own image, cache and volume trims, and applies in `apply` mode, in the run that holds the lock | `POLECAT_IMAGE_REGISTRY_PRUNE` |
| `gt maint plan [registry]` / `gt maint apply [registry]` | plans by hand, or applies the plan it just printed; a name it does not know is listed back | none: `plan` never applies, `apply` always does |
| the `disk_maintenance` patrol | plans every target in a quiet daily window, and applies in `apply` mode | `daemon.patrols.disk_maintenance` |

`registry` is the only target in slice 1, and naming no target runs every
target the town has. `plan` is the dry run, so it creates neither the ledger
directory nor anything else; both `plan` and `apply` exit non-zero on a target
that refused or failed.

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
- the newest `POLECAT_IMAGE_REGISTRY_KEEP` candidate tags by manifest creation
  time.

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

## Dry run and apply

Every entry point starts in dry run, and none of them can be flipped on by
default.

- `POLECAT_IMAGE_REGISTRY_PRUNE` is `off`, `dry-run` (the default) or `apply`,
  and gates the plugin only.
- `daemon.patrols.disk_maintenance.mode` is `dry_run` (the default) or `apply`,
  and gates the patrol only. The patrol also needs `enabled: true`; on its own
  it plans and never applies.
- `gt maint apply` applies because it was typed.

## The ledger

Every apply appends one JSON line to `<town>/.runtime/diskmaint/ledger.jsonl`
(`~/gt/.runtime/diskmaint/ledger.jsonl` in the default town). When a line would
push the live file past a mebibyte it rotates to `.1` first, discarding an older
rotation. A `plan` writes nothing, so the ledger is the record of what was
applied.

```json
{"ts":"2026-10-29T04:31:07Z","target":"registry","mode":"apply","planned":4,"done":4,"bytes_freed":8589934592}
```

A line names the target, the mode, how many actions were planned and done, the
bytes freed, and any refusal, deferral or error, including why garbage collect
was skipped. `tail -n 1 ~/gt/.runtime/diskmaint/ledger.jsonl` after a run is
the quickest check that it did what the plan said.

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
candidate tags instead of the configured 5; a smaller configured keep stays
where it is. A disk that could not be read refuses that day's pass outright — a
keep decided without it would be a guess — and the window counts as settled, so
the next pass is the next day's. A deferred apply is the one conclusion that
leaves the window open, because the push holding the lock is what to wait for.

## Rollback

A wrong delete costs a rebuild, not data: any per-commit image is rebuilt from
its commit (`make polecat-image` in a worktree of that commit) and republished
by the next plugin run. To stop pruning at once, set
`POLECAT_IMAGE_REGISTRY_PRUNE=off` for the plugin and `enabled: false` for the
patrol, which also stops the disk alerts. The ledger names every manifest
digest deleted and when.

The switches, their defaults and where each is read are in
[the configuration reference](../reference.md#disk-maintenance).
