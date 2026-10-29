> Status: historical (2026-10). Merged in: gt-i7hxu (design and plan; the implementation beads are filed from the plan). Not maintained.

# Town disk maintenance: a tested Go pruner, registry retention first

Date: 2026-10-28. Author: overseer.
Related: gt-mexpf, gt-atxdz, gt-3g7p2, gt-e14j6, gt-06ni7 (the prunes the `polecat-image` plugin already runs), the `scheduled_maintenance` patrol (Dolt backup and gc window).

## Problem

Things that fill disks are pruned in scattered places, and the biggest one is not
pruned at all.

- The sandbox registry `gt-registry` (`registry:2`, in the Colima `gt-sbx` VM,
  `localhost:5001`) holds 63 `gastown-polecat` tags, 51.9 GiB, plus two stray
  repos (`gastown-polecat-measure`, `seedtest/polecat`). Every install pushes a
  ~2 GiB image. Nothing deletes a tag, and until 2026-10-28 the registry had
  deletes switched off. Colima's data disk (158 GiB after the same-day resize)
  was 69% full before it grew.
- What is pruned today lives in three places: the `polecat-image` plugin
  (`internal/polecatimage`: old per-commit tags on Docker Desktop, the build
  cache, per-rig cache volumes), `cmd/nodeprune` (kind-node images, off unless
  `POLE_NODE_PRUNE=1`), and shell or handbook rules (`trim-runner-caches.sh`,
  the weekly `.bak` and old-binary rules, worktrees by hand).
- Nothing watches the disks the registry and the VMs live on. The 85% rule in
  the docker-watch runbook covers the CI runner volume only.
- Crew worktrees pile up: 96 under `~/gt/gastown/crew` (5.0 GiB), 11 of them
  stale overseer worktrees from earlier sessions.

## Goals and non-goals

Goals: delete what is safe to delete on a schedule, with evidence in a ledger;
alert before a disk fills; keep the logic in Go with tests so it is not a script
nobody can test; make the next pruner a small addition, not a new mechanism.

Non-goals: pruning anything an operator has to decide (Dolt directories,
`~/gt-backups`, cache-trash directories) beyond reporting its size; replacing the
existing `polecat-image` prunes in slice 1 (they become targets later); cloud
registries (devops owns the Artifact Registry policies).

## Architecture

One package, `internal/diskmaint`, three ways in.

```
                 +-------------------------------+
 polecat-image --|                               |
 (after trims,   |   diskmaint.Run(target, opts) |-- ledger.jsonl
  under its lock)|     Measure -> Plan -> Apply  |   ~/gt/.runtime/diskmaint/
 daemon patrol --|                               |-- alert (daemon only)
 (daily, quiet)  +-------------------------------+
 gt maint -------/        |
 (plan|apply)        Target implementations:
                     registry (slice 1), worktrees, backups, nodes, ...
```

```go
// Target is one thing that fills a disk and can be pruned.
type Target interface {
    Name() string
    Measure(ctx context.Context) (Measurement, error) // bytes used, budget, detail
    Plan(ctx context.Context, p Policy) (Plan, error)  // pure decisions, no writes
    Apply(ctx context.Context, plan Plan) (Result, error)
}

type Plan struct {
    Target  string
    Actions []Action // Kind ("delete-manifest", "delete-repo", "remove-worktree", ...), Ref, Bytes, Why
    Kept    []Kept   // Ref and the rule that kept it, so a dry run explains itself
    Refused string   // non-empty: the plan is empty on purpose, and why
}
```

Rules that hold for every target:

- **Dry-run is the default.** `Apply` runs only when the caller says so.
- **Fail closed.** An input that cannot be read (a listing, the in-use set, the
  installed commit) gives an empty plan with `Refused` set. It never gives a
  guess.
- **Every run writes a ledger line** (`ts`, target, mode, actions planned and
  done, bytes freed, errors) to `~/gt/.runtime/diskmaint/ledger.jsonl`. The
  ledger is the only state; there is no database.
- **All I/O is behind seams** (an HTTP base URL, a command runner, a clock, a
  filesystem view), so every rule is a unit test.

## The registry target (slice 1)

### What it talks to

The registry's v2 HTTP API at the configured address (default
`http://localhost:5001`): `GET /v2/_catalog`, `GET /v2/<repo>/tags/list`,
`GET /v2/<repo>/manifests/<ref>` (digest from `Docker-Content-Digest`, creation
time from the config blob's `created` field; for an index, the newest child), and
`DELETE /v2/<repo>/manifests/<digest>`. Garbage collect runs as
`docker exec gt-registry registry garbage-collect --delete-untagged
/etc/docker/registry/config.yml` on the Colima daemon
(`DOCKER_HOST=unix://$HOME/.colima/gt-sbx/docker.sock`, read from the town's
`settings/config.json` the way `polecat-image` reads its registry settings).

### Retention is by digest, not by tag

A registry delete removes a manifest by digest, and that drops **every tag that
points at it**. `dev` and the installed commit's tag usually share a digest.
So the planner works on digests:

1. Candidates are tags of `gastown-polecat` that look like a short commit hash
   (hex, 7 to 12 characters). Every other tag, `dev` included, is never a
   candidate.
2. The keep set of digests is the union of: the digest `dev` points at; the
   digest of the tag that is a prefix of the installed gt commit (`gt version`
   is the one source for that commit); the digest of every image a running
   sandbox pod uses, by `imageID` and also by the tag its container `image`
   field names (an `imageID` can be a config digest or a bare image id, so the
   tag is resolved through the registry too when it names `gastown-polecat` or
   a stray repo, the only repos a delete can touch; the cluster's own images are
   not resolved); and the digests of the newest `KEEP` candidate tags by
   manifest creation time (default 5). If `dev` is
   absent from `gastown-polecat`, the plan refuses: a registry without `dev` is
   mid-rebuild or broken, and a prune would have nothing to anchor on.
3. A manifest is deleted only when its digest is outside the keep set. Because
   the keep set is by digest, an old candidate tag that shares a digest with a
   kept tag (`dev`, say) is not deleted: deleting it would delete the kept tag
   too. A digest with no kept tag pointing at it goes, and with it every older
   tag that named it.
4. The in-use set comes from `kubectl --kubeconfig ~/gt/.runtime/sbx-k8s/kubeconfig
   get pods -A -o json` plus the sandbox records under `~/gt/.runtime/sandboxes`.
   If either cannot be read, **including a records directory that does not
   exist**, the plan refuses: a town that runs sandboxes has that directory, so
   its absence means something is wrong.
5. Stray repos (`gastown-polecat-measure`, `seedtest/polecat`) are deleted whole,
   only when nothing in the in-use set or the sandbox records names them.
6. Garbage collect runs only if at least one delete succeeded, and never when the
   registry reports a push in progress. **Every apply, from the plugin, the
   patrol or `gt maint`, runs inside `polecatimage.WithLock`**, the same lock that
   serializes every push; if the lock is held (a build or push is running) the
   apply defers and says so. A dry run needs no lock.
7. Every name that comes from the registry or from the sandbox records is
   validated before it reaches a URL path or a command: a repo matches
   `^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`, a tag
   `^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`, a digest `^sha256:[0-9a-f]{64}$`, and the
   container name `^[A-Za-z0-9][A-Za-z0-9_.-]*$`. A name that fails refuses the
   plan. The garbage-collect command is fixed text plus the validated container
   name.

Recovery is a rebuild: any per-commit image can be rebuilt from its commit, so the
newest-N window is a convenience, not the only copy.

## Disk watch and the daily patrol

A new daemon patrol, `daemon.patrols.disk_maintenance`:

```json
{ "enabled": true, "interval": "daily", "window": "04:30", "mode": "dry_run" }
```

- It runs in a quiet window after the Dolt window (`scheduled_maintenance` uses
  03:00) and skips while the `polecat-image` lock is held or a landing pass is in
  flight, using the same quiet test `rebuild_gt` uses.
- It measures three disks: the Mac volume (`df`), Colima's data disk
  (`colima ssh -p gt-sbx -- df -P /mnt/lima-colima-gt-sbx`), and Docker Desktop's
  disk (`df` of the runner volume, as the docker-watch rule does). At 80% used it
  raises a keyed alert (`diskmaint:<disk>`) through the daemon's existing
  `escalateAlert`, and clears it when the disk is back under 70%. A disk it
  cannot read raises its own alert (`diskmaint:<disk>:unreadable`) and clears
  when a later read works: a disk we cannot measure is not a quiet disk.
- Over 80% the registry `KEEP` tightens from 5 to 3 for that run.
- In `apply` mode the patrol takes the same `polecatimage.WithLock` before it
  applies, and defers when the lock is held.
- `mode` is `dry_run` until the operator flips it to `apply`. The plugin hook has
  the same switch (`POLECAT_IMAGE_REGISTRY_PRUNE=off|dry-run|apply`, default
  `dry-run`).

## Entry points

- **`polecat-image`**: `Program()` calls the registry target after the existing
  trims (`PruneOldImages`, `PruneBuildCache`, `TrimCacheVolumes`), still holding
  the lock. The first step is one `tags/list` call; manifests are fetched only
  when the tag count exceeds `KEEP + 1`, so a 10-minute cooldown run costs one
  request when there is nothing to do.
- **`gt maint plan|apply [target]`**: a thin command in `internal/cmd` that
  builds the targets from the town config and prints the plan or applies it.
  `plan` is the dry run.
- **The patrol**: described above.

## Configuration

| Knob | Default | Where |
|---|---|---|
| `POLECAT_IMAGE_REGISTRY_KEEP` | 5 | plugin env / town `settings/config.json` |
| `POLECAT_IMAGE_REGISTRY_PRUNE` | `dry-run` | same |
| `daemon.patrols.disk_maintenance` | `dry_run`, daily 04:30 | town `settings/config.json` |
| alert / clear thresholds | 80% / 70% | constants, tested |

## Infrastructure change (already done by hand, to be made permanent)

`deploy/k8s/kind/up.sh` starts the registry as `docker run -d --restart=always -p
127.0.0.1:5001:5000 --name gt-registry registry:2`: no volume flag (the data is
an anonymous volume) and no delete flag. On 2026-10-28 the container was
recreated by hand with `REGISTRY_STORAGE_DELETE_ENABLED=true`, the same data
volume mounted explicitly, and the `kind` network reconnected; tags and digests
were verified identical. A bead makes `up.sh` and `kind_test.sh` match: a named
volume `gt-registry-data` and the delete flag, so a rebuild keeps both.

## Testing

- Planner: table tests over fake listings, covering a tag sharing `dev`'s digest,
  an in-use digest, the newest-N cut at the boundary, a non-hex tag, an empty or
  failed listing (empty plan, `Refused` set), and stray repos referenced and
  unreferenced.
- Client: `httptest` server for the catalog, tags, manifest and delete calls,
  including an index manifest and a 405 (deletes off) answer, which refuses.
- Apply: a fake runner for garbage collect; a delete that fails keeps going for
  the others, is recorded, and still triggers collect only if one succeeded.
- Each safety rule has a mutation check: the shared-digest guard, the in-use
  guard, and the non-hex guard each turn a named test red when removed.
- The patrol and the command are tested with the daemon and `cmd` packages'
  narrow `-run` filters, as those packages are container-backed.

## Later slices

| Slice | Target | Rule |
|---|---|---|
| 2 | Worktrees | Auto-remove only `~/gt/<rig>/crew/overseer-*` worktrees that are clean, whose branch is pushed or landed, older than 2 days. Every other crew or polecat worktree is report-only with owner and age. |
| 2 | `.bak` files and old binaries | The handbook's pre-approved rules, ported to Go: keep the union of the newest 30 and every copy under 7 days; for binaries keep the two newest `*.bak-*`; never touch a file a process has open. |
| 3 | Colima and kind-node images | Fold `cmd/nodeprune` and the Colima `docker image prune` in as targets; resolves the opt-in `POLE_NODE_PRUNE` question. |
| 3 | CI volumes | Port `trim-runner-caches.sh` (whole-directory module trims) to a target. |
| - | Report only | Dolt `.bak` directories, cache-trash directories, `~/gt-backups`: size reported, never deleted, until the operator sets a retention. |

## Risks

- **Registry delete is irreversible.** Mitigations: the digest rule, the in-use
  set, `dev` and the installed commit always kept, dry-run first, and a rebuild
  path for any image.
- **Garbage collect while a pull is running.** A pull reads blobs that collect
  may remove if they became unreferenced. Mitigation: only delete manifests
  whose digests no kept tag shares, and run collect in the quiet window or under
  the plugin lock.
- **Alert noise.** One keyed alert per disk, cleared at 70%.

## Out of scope

Cloud registries, Dolt retention, any change to the polecat image build itself,
and the order in which the later slices ship (the plan sets it).
