# bd runs as its own container behind a gateway

Status: draft for the overseer's vetting, written 2026-11-13 (dipper). Program
bead: gt-2zigh5 (operator decisions 1 to 9, 2026-11-13). Related designs:
[gt runs from the daemon image](gt-in-container.md), [the town tracker
plan](../plans/2026-10-25-town-tracker-design.md).

Nothing here is built. Facts below were read from origin/main at 440acced9 and
from the live town on 2026-11-13; "not verified" marks what was not checked.

## Goal

The `bd` binary leaves the daemon image and the polecat image. One dedicated bd
image, built by CI, runs as a long-lived container in the Mac's local Docker
beside Dolt, in one compose unit started at system startup. Every caller that
runs bd today calls that container instead, through a small authenticated
gateway, and the bd shim (tracker routing) lives in the same image. Seats keep
their own bd until approach A (TASK.md in, outbox out, gt-4fgpu5) is built.

## Facts this design rests on

- One seam. Every gt call of bd goes through `bdRunFunc(ctx, bdCall{bin, dir,
  env, args, stdin})` in `internal/beads/runner.go`; `bdBinary(bin)` names `bin`
  or `bd` on PATH (runner.go:58); the pinned path builds the command in
  `newBDCmd` (beads.go:1835). A binary named `bd` that forwards needs no change
  in any caller of that seam.
- Outside the seam, raw `exec.Command("bd")` calls remain, all in
  `internal/beads`: exec.go:148 and :156 (the `BdCmd` and `CommandWithEnv`
  helpers many `internal/cmd` callers use), beads_types.go (config set and list,
  init, migrate), wisp_cleanup.go:42. `exec.LookPath("bd")` probes sit at
  beads.go:160, daemon.go:1607, doctor/bd_client.go:47, patrol_scan.go:1304.
  Other users: internal/mail/bd.go, internal/metrics/bdsnap.go (`bd sample`),
  townstate/migrate.go (`bd list --limit 1`). hm-sync's own use of bd is not
  verified.
- Every path is absolute. gt sets `BEADS_DIR` to an absolute town path and the
  shim finds `routes.jsonl` by walking up from it. Today the town path is
  `/Users/sloan/gt` on the Mac and in the pod.
- The pod's town is NOT the Mac's town. The pod mounts the `town-state` claim at
  `/Users/sloan/gt` (daemon Deployment volumes). After the one-town-copy cutover
  (gt-qq5kv4) the claim is the only town and the Mac keeps none. A container in
  the Mac's Docker cannot mount a claim that lives in the control VM. This is
  the central design question; see [The workspace](#the-workspace).
- Volume (pod daemon log, 2026-11-13 11:36 to 14:56 CST): the spec dispatch tick
  runs about once a minute with a median of 27 bd calls (min 21, max 228);
  daemon-wide 60 s windows have median 23 and maximum 90; about 50 to 60 calls a
  minute overall. `daemon.listLocalRoundRequests` alone is 10 a minute.
- Latency today: in the pod bd show and list take 35 to 40 ms warm and 866 ms
  on the first call of a process; the Mac wrapper takes 304 ms per `bd show`.
  The "225 ms" figure from the program bead appears in no record I could find;
  do not budget from it.
- A host-gateway hop is cheap: a GET from the pod to `192.168.5.2:5001`
  measured 2.7 to 4.7 ms (five samples).
- bd as the town builds it is a pure-Go Dolt client: linux/arm64,
  `CGO_ENABLED=0`, `-trimpath`, no build tags, stamped `main.Commit` and
  `main.Build` (internal/polecat/bdbuild/bdbuild.go:20-26, daemonimage/build.go
  :275). It cannot run embedded Dolt and does not need to.

## Architecture

```
 Mac callers                        Docker Desktop (Mac)                    Colima VMs
 -----------                        --------------------                    ----------
 bd (gt-wrap, docker target)  --docker exec--> [ bd container "gt-bd" ]
                                                  bdgw  (gateway)      <--HTTP, bearer-- daemon pod, dashboard pod
                                                  bd    (the shim)                        (bd = gt-wrap, bdgateway target,
                                                  bd.real                                  via 192.168.5.2:7440)
                                                  workspace tree  -----------> Dolt  [ "gt-dolt" container, same compose unit ]
                                                  tracker client  -----------> trackerd Service (via 127.0.0.1:7433 forward)
```

- `bdgw` is one static Go binary (package `internal/bdgateway`, command
  `cmd/bdgw`). It is the container's entrypoint. It has two modes sharing one
  policy, one auth and one call log: `bdgw serve` (HTTP) and `bdgw exec -- <bd
  args>` (local, for the Mac wrapper's docker target, so a Mac call is checked
  and logged the same way).
- Inside the container `/usr/local/bin/bd` is the shim (`cmd/bdshim`) and the
  real bd is `/usr/local/bin/bd.real` beside it, exactly the layout
  `internal/bdshim/osmain.go` looks for. The gateway only ever runs `bd`.
- The bd container reaches Dolt over the compose network by service name, never
  through a published port.
- The gateway holds one tracker credential: an operator-class tracker token
  with the add-as capability (gt-qr8pe), in a mounted file named by
  `TRACKER_TOKEN_FILE`. The actor on a tracker write comes from the caller's
  `BD_ACTOR`, which the gateway has authenticated by token class. This removes
  the per-actor tracker token the flip left open: callers hold gateway tokens,
  the gateway holds the one tracker token. It makes gt-qr8pe a prerequisite of
  the first tracker rig behind the gateway.

### Call path from each caller

| Caller | Today | After |
|---|---|---|
| Mac shell, scripts, hooks | gt-wrap kubernetes target: `kubectl exec` into the daemon pod, 304 ms | gt-wrap docker target, configured for the bd container: `docker exec gt-bd bdgw exec -- <args>` |
| Daemon patrols, spec dispatcher, landing worker, sling, done | `bd` on PATH in the pod, exec per call | `bd` on PATH is gt-wrap with the `bdgateway` target: HTTP to `http://host.docker.internal:7440`, class `daemon` |
| Dashboard Deployment | `internal/beads` in its own pod | same forwarder, class `read` |
| Plugins (town-metrics, app-promote), hm-sync | inherit the daemon pod's `bd` | same forwarder; hm-sync's bd use is not verified |
| gt commands run by sessions through gt-wrap in the pod | bd inside the pod | the forwarder, class `daemon`; the actor is logged from `BD_ACTOR` |
| Polecat seats (sandboxes) | their own bd, Dolt via policy endpoint | unchanged until approach A; a gateway endpoint would need a new policy entry in deploy/polecat/policy.yaml and deploy/k8s/policy/kind.yaml |

A forwarder per call cannot keep an HTTP connection alive. The first phase
accepts that (one extra TCP connect, about 3 to 5 ms). A later, optional step
gives the daemon an in-process client behind `bdRunFunc` with keepalive; it needs
no protocol change.

## The gateway protocol

One endpoint does the work; two more are for health and the workspace.

```
POST /v1/exec
  Authorization: Bearer <token>
  Content-Type:  application/vnd.gt.bdexec+json   (the request head) 
  X-Gt-Caller:   <free text, logged: "daemon.spec-tick", "mac.sloan-shell">
  body:          one JSON head line, a newline, then the caller's stdin bytes
```

Head (JSON):

```json
{
  "argv":    ["show", "gt-abc", "--json"],
  "env":     {"BD_ACTOR": "town/crew/dipper", "BD_JSON_ENVELOPE": "1"},
  "rig":     "gastown",
  "cwd":     "gastown/crew/dipper",
  "timeout_ms": 30000
}
```

- `argv` is bd's arguments, never `bd` itself, never a shell string; the gateway
  runs the process with an argv vector, no shell.
- `rig` and `cwd` are relative to the town root. The forwarder computes them from
  its own absolute `BEADS_DIR` and working directory with the town-root mapping
  gt-wrap already has (`pod_town_root`, internal/gtwrap), so the caller's
  absolute path is never sent and the container needs no same-path mount.
- `env` is an allowlist, below. Anything else is refused, not dropped, so a
  caller learns it sent something the gateway does not take.
- stdin streams after the head line. The gateway caps it at 1 MiB (a bead body
  is the largest user: `--body-file=-`). The forwarder keeps the file-argument
  rewrite `internal/gtwrap/filearg.go` already does: `--body-file`,
  `--description-file`, `--design-file`, `--reason-file`, and `-f` on
  `comments add`, `note`, `human respond` and `dep add` become `-` and the file
  travels as stdin; a call with two files, or a file beside `--stdin`, is
  refused before any request.
- Response: HTTP 200 with a stream of frames, each one kind byte, a four-byte
  big-endian length, then the payload. Kinds: `1` stdout, `2` stderr, `3` exit
  (payload: the exit status as ASCII digits, always the last frame), `4` gateway
  error (payload: JSON `{"code":"...","message":"..."}`, followed by an exit
  frame with status 125). bd's stdout and stderr are therefore separate and in
  order, and the exit status passes through unchanged, as gt-wrap does today.
- Refusals are HTTP before any frame: 401 no or bad token, 403 verb or flag not
  allowed for the class, 400 malformed head, 413 stdin over the cap, 429 the
  concurrency cap (default 16 running bd processes) is full, 504 timeout.
- Timeouts: the head's `timeout_ms` is capped by the class maximum (reads 30 s,
  writes 60 s, `list` and `ready` on a large rig 120 s). On timeout or on the
  client closing the connection the gateway kills bd's whole process group and
  returns frame 4, then exit 124. An orphan bd must never outlive its request.
- `GET /healthz` (no token): 200 when the process serves; `GET /readyz` (token,
  class `read`): 200 only when it can run `bd --version` and read `routes.jsonl`
  from the workspace. The compose health check uses `readyz` from inside the
  container.
- `PUT /v1/workspace` (class `daemon` only): see [The workspace](#the-workspace).

### Environment allowlist

Accepted from the caller: `BD_ACTOR`, `BEADS_ACTOR`, `BD_MACHINE`,
`BD_JSON_ENVELOPE`, `BD_READONLY`, `BD_DOLT_AUTO_COMMIT`. Computed by the gateway
and never accepted: `BEADS_DIR` (from `rig`), `BEADS_DOLT_SERVER_HOST` and
`BEADS_DOLT_SERVER_PORT` (the compose service name and port), `TRACKER_URL`,
`TRACKER_TOKEN_FILE`, `HOME`, `PATH`. `BD_ALLOW_NEW_DATABASE` is refused with
every class: `init` is not served. The class forces `BD_READONLY=true` for
`read`, whatever the caller sent.

### Token classes

Tokens are 32-byte random values in files, compared in constant time; the
gateway reads a directory of `<class>.token` files and reloads on `SIGHUP`, so
rotation needs no restart.

| Class | Used by | May run |
|---|---|---|
| `read` | dashboard, monitors, anything that only looks | the read verbs |
| `daemon` | daemon, landing worker, plugins, wrapped gt in the pod | read verbs and the write verbs gt makes; may call `PUT /v1/workspace` |
| `operator` | the Mac wrapper (docker target needs no token; the token is for a future network Mac target) | everything `daemon` may, plus `config set` for `issue_prefix` |

Not served to any class: `init`, `migrate`, `dolt` and `backup` administration,
`sql`, `mail`, `hooks`, `edit`, and any flag that makes bd open an arbitrary path
or point at another database (`--db`, `--dolt-*`, `--sandbox`). These stay a
`docker exec gt-bd bd.real ...` by the overseer.

### The verb allowlist

The program bead's "45 of 118" comes from the tracker design (line 21: 80,000
recorded calls on 2026-10-25) and the design does not list the 45 verbs; the
recorded-call table is not verified to survive. The list is therefore built, not
quoted, in task G2, from three sources that exist on main:

1. the shim's 23 handlers (`internal/bdshim/osmain.go:14`): `show list children
   ready blocked query search count create update close reopen delete defer
   undefer unclaim comments label kv dep config events stats`, with `cook` and
   `formula` passed through to bd.real;
2. the daemon's call sites from the bd census lines in `daemon.log` (the top
   callers are `daemon.listLocalRoundRequests`, `landworker.landReady`,
   `landworker.sweepBeadDone`, `daemon.showRigBead`, `rules.rigDones.Done`,
   `land.openDescendants`, `schedulerrun.listAllSlingContextRecords`,
   `rig.getBeadLabel`, `submission.rigBeads.Since`, the plugin Recorder's
   create and close, `writeComment`), plus `sample add/list`, `config get`, and
   the `--allow-stale version` probe gt makes once per binary path;
3. the verbs the shim refuses or does not yet answer (`mol`, `wisp`, `gate`):
   included only if the census shows a daemon caller, and then as `daemon`
   class pass-through to bd.real.

The result is `deploy/bd/verbs.json` (verb path, class, flags refused), checked
by a test against `internal/cmdtree/bd-command-tree.json` (every allowed verb
exists in the tree; every shim handler verb is allowed) so a bd upgrade that
renames a verb fails the gate instead of the daemon.

### The call log

One JSON line per call to a file in a mounted log volume, rotated at 50 MB with
seven files kept: time, token class, `X-Gt-Caller`, `BD_ACTOR`, rig, the verb
path, the flag names (not values) and bead ids found in argv, stdin byte count,
stdout and stderr byte counts, exit status, duration, whether the call was served
by the shim or by bd.real. Free-text values (titles, descriptions, comment
bodies) are never logged. `GET /v1/stats` (class `read`) returns counts and
latency percentiles per verb and caller for the last hour, which is the evidence
for revisiting the 1 CPU and 1 GiB limit (decision 7).

## The workspace

bd reads four files per rig and one per town: `<rig>/.beads/redirect`,
`metadata.json`, `config.yaml`, `dolt-server.port`, and `routes.jsonl` at the
town's `.beads/` (seagull's facts, internal/sandbox/skeleton.go). They are
written by gt (`EnsureMetadata`, `EnsureMetadataForBeadsDir`, `WriteRoutes`, rig
setup) and by `bd init` and `bd config set`. A route flip is an edit to
`routes.jsonl`; the bd container must see it at once.

Two ways to give the container those files:

- A. Mount the Mac's `~/gt` at the same path. Simple, and the paths mean the
  same thing. It fails the moment the Mac stops being a town (gt-qq5kv4), puts a
  read-write mount of the whole town in a network-facing container, and the Mac
  copy and the pod's claim are already two copies that can drift.
- B. The gateway owns a small workspace tree in its own volume, `/ws/<rig>/.beads/
  {metadata.json, config.yaml, redirect, dolt-server.port}` and `/ws/.beads/
  routes.jsonl`, and gt pushes it. **Recommended.**

Under B:

- gt-side, `gt bd workspace push` reads the files from the town copy it runs in
  (the pod's claim) and sends them with `PUT /v1/workspace` as one JSON document
  `{"manifest_sha":"...","files":{"<rel path>":"<content>"}}`. The gateway
  validates every path (relative, no `..`, only the five file names above),
  writes into a temp tree, renames it into place atomically, and answers with
  its own manifest hash. Paths inside `metadata.json` and `redirect` are rewritten
  to `/ws` relative form on the gt side.
- It is called, always, after the function that changes the files returns:
  `EnsureMetadata*` and `WriteRoutes` call it through a small `workspacePusher`
  interface (nil when no gateway is configured, so every existing test and the
  Mac-only paths are untouched), the daemon calls it at start and every five
  minutes as a reconcile, and the tracker flip procedure calls it after the
  routes edit.
- Every gateway response carries `X-Bd-Workspace: <manifest_sha>`. The forwarder
  compares it with the hash of the files it believes current (the daemon knows;
  other callers skip the check) and the daemon's doctor reports a mismatch as
  drift, which is how staleness is noticed, not only prevented.
- The tracker shim needs the same tree: it finds `routes.jsonl` by walking up
  from `BEADS_DIR`, which the gateway sets under `/ws`.
- Dolt's own databases are not in the workspace; they stay in Dolt.

Open: how `bd init` (a rig add) works under B. Init is not served over the
network; `gt rig add` runs it once with `docker exec gt-bd bd.real init` against
a temp workspace and then pushes the result, in task G8. Not verified: that bd
accepts a workspace whose `redirect` was rewritten; the first gateway task proves
it with a real bd before anything else is built on it.

## Latency budget

Baseline: 35 to 40 ms warm per bd call in the pod (866 ms for the first call of
a process), 304 ms through the Mac wrapper. A tick makes 21 to 228 calls
(median 27), a minute 50 to 60, bursts to 90 in a 60 s window.

Budget for the gateway path, measured from the forwarder's `exec` to its exit:
overhead over today's in-pod call at most 15 ms at the median and 40 ms at the
95th percentile, with a hard failure at 200 ms. At 15 ms a median tick adds
0.4 s; the 228-call worst tick adds 3.4 s, which the in-process keepalive client
brings back under 1 s. The measured hop is 3 to 5 ms, the spawn is the same as
today, so the budget leaves about 10 ms for authentication, policy, the call log
write and frame copying; the call log writes through a buffered channel, never
inline. The Mac path should get faster, not slower: `docker exec` into a
resident container replaces a `kubectl exec` that cost 304 ms.

The measurement that decides is task G13: a load run of 200 `show` calls and 50
`list --json` calls through the gateway from the daemon pod, recorded as p50, p95
and p99 against the same calls through the in-pod bd.

## Images and CI

Two layers (decision 6), because the shim is under gastown's `internal/bdshim`,
which another module cannot import.

1. **Base image `bd`, built by the beads rig.** A `Dockerfile` in the beads repo:
   Debian slim, `tini`, a non-root user, the town-style bd (linux/arm64,
   `CGO_ENABLED=0`, no tags, stamped with the commit and build id) at
   `/usr/local/bin/bd.real`, label `org.gastown.bd-commit`. The beads repo has
   `.forgejo/workflows/gate.yml` (push to `land/**`) and `post-land.yml` (push to
   main) and nothing that builds an image; `.github/workflows` is upstream CI,
   inert here. A new `.forgejo/workflows/bd-image.yml` builds and pushes
   `forgejo:3000/bot-registry/beads-bd` on a push to main and tags it with the
   commit.
2. **Gateway image `gt-bd`, built by gastown's image CI.** `Dockerfile.bd` in
   deploy/bd: `FROM forgejo:3000/bot-registry/beads-bd@<digest>` (the digest from
   a pins file, never a tag), adds `bdgw` and the shim, moves `bd` to `bd.real`
   if the base did not, sets the entrypoint. `images.yml` already builds the
   daemon and polecat images on a push to `green/**` and `imagepublish check`
   reads `org.gastown.gt-commit` back; it gains a third image.

The pin: `deploy/values/images.yaml` (written by `gt update images`, read by
`internal/imagevalues`) gains a `bd:` key holding the gateway image's digest;
the base image's digest sits in `deploy/bd/base-pin`. The bump step: after the
beads CI pushes a new base, `gt update bd-base` resolves the `beads-bd:<commit>`
tag to a digest, writes the pin, and the next gastown image build picks it up;
`gt update images` then writes the new `bd:` digest. The compose unit reads the
digest from the values file; nothing runs `:latest`.

Docker Desktop must be able to pull from the Forgejo registry. The cluster
pulls from `forgejo:3000/bot-registry`; whether Docker Desktop's daemon can
resolve and trust that name is not verified, and it is the first thing the
compose task checks (insecure-registry entry or a pull through `localhost`).

### Hands the operator must supply

The Forgejo hook `20-gt-protect-workflows` admits only the operator's account to
`.forgejo/workflows/`, so each workflow change is a reviewed text the operator
copies:

1. beads repo: new `.forgejo/workflows/bd-image.yml` (reviewed text kept in the
   repo, as post-land.yml does).
2. gastown: `.forgejo/workflows/images.yml` gains the bd-layer build, push and
   check steps (a test already holds the live copy equal to the reviewed text).
3. beads repo secrets `REGISTRY_TOKEN` (and `SOURCES_TOKEN` if the build fetches
   another repo), set in the Forgejo UI; the registry user is `bot-registry`.
4. the three gateway tokens and the tracker add-as token: created on the host,
   never printed, put in the secrets directory the compose unit mounts and in a
   Secret for the daemon and dashboard pods.

## The compose unit

`deploy/data/docker-compose.yml`, project `gt-data`, replaces the container
`gt doltserver` creates today with `ContainerPlan` (internal/doltserver/
containerplan.go). Today's `gt-dolt`: image `dolthub/dolt-sql-server:2.7.2@sha256:
4208ed89...`, command `--config /Users/sloan/gt/.runtime/dolt-container/config.yaml`,
restart `unless-stopped`, memory 8 GiB with `memory-swap` equal, published
`127.0.0.1:3307`, bridge network, no health check, one label (`org.gastown.keep`).
Mounts: an anonymous volume at `/var/lib/dolt`, `/Users/sloan/gt-backups/dolt`,
`/Users/sloan/gt/.dolt-data` (same path inside), and the config file read-only.

| Service | Image | Limits | Ports | Mounts |
|---|---|---|---|---|
| `dolt` (container name `gt-dolt`, kept so the ExternalContainer setting and the slot reaper's `org.gastown.keep` label still match) | the same digest-pinned dolt image | 8 GiB, swap equal, as now (the cap is never raised) | `127.0.0.1:3307:3306` | the same three bind mounts; `/var/lib/dolt` becomes a named volume (see Migration) |
| `bd` (container name `gt-bd`) | the `gt-bd` image by digest | 1 CPU, 1 GiB at the start (decision 7); revisit from `/v1/stats` | `127.0.0.1:7440:7440` | the token directory (ro), the log volume, the workspace volume, no town mount |

- Start order: `bd` `depends_on` `dolt` with `condition: service_healthy`. The
  Dolt health check is `dolt --host 127.0.0.1 --port 3306 -u root sql -q 'select
  1'` (the image has the client; the check is new, the container has none today).
  The bd health check is `bdgw ready` (the `readyz` logic locally).
- Startup at boot: no gt launchd job starts Dolt today; Docker Desktop's login
  item restarts it through `restart: unless-stopped`. The unit keeps that: both
  services use `restart: unless-stopped`, and no new launchd job is added. The
  Colima VMs and kind nodes start from `com.gastown.colima-vms` as now; the
  callers in them reach the gateway at `192.168.5.2:7440`, which reaches the
  Mac's `127.0.0.1` the way 3307, 7433 and 5001 do (probed from the daemon pod
  and from gt-sbx-worker2 on 2026-11-13: all connected).
- "Bound to the Mac host, reachable only from the Colima VMs" cannot be had from
  the network alone: Docker Desktop publishes on the Mac's `127.0.0.1`, which
  every local process reaches, and binding wider would expose it on the LAN. The
  controls are the bearer token (0600 files), the verb allowlist and the call
  log. The design says so rather than promise a network boundary it does not
  have.
- `gt dolt start`, `stop` and `status` operate the unit (`docker compose -p
  gt-data`), and `gt doctor` gains a check that both services are healthy, the
  gateway answers `readyz` with the daemon token, and the three token files
  exist with mode 0600.

## Migration and rollback

Stage 0 and 1 need no caller change and can be rolled back by deleting the new
container.

0. CI builds the base and gateway images; the compose unit file lands; the unit
   is not started.
1. **bd container only.** Start `bd` alone from the unit, pointing at the
   existing `gt-dolt` through `host.docker.internal:3307`. Push the workspace.
   Run the load measurement. Rollback: `docker compose -p gt-data down`.
2. **Fold Dolt into the unit**, in a window with the dispatch hold on and the
   queue drained (the same window rules as the tracker flip): back up Dolt
   (`~/gt-backups/dolt/<date>`), stop `gt-dolt`, copy the anonymous
   `/var/lib/dolt` volume into the named volume (`docker run --rm -v
   <old>:/from -v gt-dolt-var:/to alpine cp -a`), start the unit. Check:
   `dolt sql -q 'select count(*)'` per database equals the pre-stop counts and
   `gt dolt status` is healthy. Keep the old volume until the next nightly backup
   passes. Rollback: stop the unit, start the old container from `ContainerPlan`
   (the code stays until Stage 6), with the old volume.
3. **Mac wrapper.** Change `~/.config/gt/wrap.json` for `bd` to the docker target
   at `gt-bd`. Low volume, operator-run, immediately reversible by restoring the
   file (the gt-wrap installer already keeps dated copies).
4. **Dashboard.** The dashboard Deployment gets the forwarder as its `bd`, class
   `read`. Rollback: the Deployment's previous image tag (its own bd is still in
   the image until Stage 6).
5. **Daemon.** The daemon Deployment gets `BD_GATEWAY_URL`, the token Secret, and
   the forwarder first on PATH. The image still carries bd, so rollback is
   deleting the env variable: the forwarder runs the local bd when no gateway is
   configured. Roll in a quiet gap and watch `/v1/stats` and the daemon's own
   census lines for a tick.
6. **Clean up** after one week of clean `/v1/stats` (no 5xx, p95 inside budget):
   [Deletions](#what-is-deleted-afterwards).

Seats stay on their own bd throughout; their move belongs to approach A.

Rollback order is the reverse of the stages, and every stage up to 5 is a config
change, not a rebuild.

## What is deleted afterwards

- The `bd` copy in `deploy/daemon/Dockerfile` (line 54) and the "build bd" step in
  `internal/daemonimage/build.go` (the build moves to the beads CI); the
  polecat image keeps bd until approach A.
- gt-q68hh2 and gt-jqrnty (the shim-in-image beads, held by gt-ow0dyw): the
  daemon one is closed as replaced; the polecat one stays valid only if seats
  keep their bd on a tracker rig, so it is re-scoped to "seats' bd is the shim"
  or closed with approach A.
- gt-wrap's kubernetes-target special cases for bd (file-argument rewrite stays,
  it moves with the forwarder; the `BEADS_DOLT_SERVER_HOST` rewrite and the
  `pod_town_root` path rewriting for bd go).
- `BEADS_DOLT_SERVER_HOST=host.docker.internal` in the pod's environment and the
  `dolt_server_port` fallback logic that exists only so the pod's bd can find
  Dolt (gt-49f3pn and gt-2r2och keep serving bd.real on the gateway side).
- `ContainerPlan` and the `gt dolt start` container creation path, after the
  unit has been the only way to start Dolt for one release.
- The `exec.LookPath("bd")` probes become a gateway readiness check.

## Risks

- **The gateway is every bd call's single point of failure.** Mitigation: restart
  policy, the health check, the fallback to local bd in the daemon image until
  Stage 6, and the daemon's doctor check. After Stage 6 a down gateway stops the
  daemon's reads; that is the price of decision 1 and it makes the health check
  and the Docker Desktop restart ordering part of the town's availability.
- **The workspace goes stale.** A route flip or a rig add changes files the
  container must see. Mitigation: push on every writer, a five-minute reconcile,
  the manifest hash on every response, doctor drift. Residual risk: a caller
  between a flip and its push; the flip procedure pushes inside its own step.
- **bd rejects a rewritten workspace.** `redirect`, `metadata.json` and the
  project id check (bd refuses on a mismatch) are fiddly. The first task proves
  one real bd against a pushed workspace before anything is built on it.
- **Docker Desktop reaching the Forgejo registry**: not verified (above).
- **The token is the only boundary** on a port every local process can reach.
  Mitigation: 0600 files in a directory outside the town tree, a rotation that is
  a file replace plus `SIGHUP`, the call log, and no token ever in an argv.
- **Logging leaks.** argv carries bead text. Mitigation: the log keeps flag names
  and ids, never values; a test feeds a secret-looking value and asserts it is
  absent.
- **Orphan processes and runaway calls.** Process-group kill on timeout or
  disconnect, a concurrency cap, a stdin cap.
- **bd upgrades rename verbs.** The verbs file is checked against the command tree
  in the gate.
- **The one-town-copy cutover changes the ground** under the workspace push (the
  Mac copy goes away). Under option B nothing in the gateway depends on which
  copy gt reads.
- **Two-image CI chain and operator-only workflow pushes** slow the first cut;
  the beads rig is parked, so the bd base image needs it unparked or a crew
  session.
- **Per-call process start** remains: the gateway still starts one bd per
  request (35 to 40 ms), so latency is not improved, only not made worse. A
  persistent bd is outside this design.

## Open decisions for the overseer and operator

1. Workspace: option B (gateway-owned tree, pushed) over option A (mount the Mac's
   town). Recommended B.
2. Whether `read` class may call `GET /v1/stats` (recommended yes) and whether the
   call log may be read from the Mac by file (recommended: a mounted log volume
   readable at `~/gt/.runtime/bd-gateway/`).
3. Whether the gateway holds the tracker's add-as token (recommended yes: it ends
   the per-actor tracker-token question) and so makes gt-qr8pe a prerequisite.
4. Whether the Mac wrapper uses the docker target (no token on the Mac, logged
   through `bdgw exec`) or the network target (token file on the Mac). Recommended
   docker, as the program bead says.

# Plan

Every task is one Gas Town bead: one rig, one deliverable, about 20 minutes,
3 to 6 acceptance boxes a polecat can tick alone. IDs are plan numbers, not
bead ids; beads are filed after the overseer vets. "Needs" names dependencies.
`[gt]` is gastown, `[be]` is the beads rig (parked: it needs unparking or a
crew session), `[op]` is a hand step that is not a bead a polecat takes.

## Phase 0: proof before building

- **P0 [gt]** A throwaway test (kept as an integration test behind the existing
  tag) that builds a workspace tree in a temp directory the way task G7 will
  push it (`routes.jsonl`, one rig's `redirect`, `metadata.json`, `config.yaml`,
  `dolt-server.port`), runs a real `bd show` against it with `BEADS_DIR` set and
  the Dolt endpoint from env, and asserts it reads a seeded bead. If bd refuses a
  rewritten `redirect`, the design changes before anything else is built.
  Needs: nothing.

## Phase 1: the gateway (all `internal/bdgateway`, `cmd/bdgw`) 

- **G1 [gt]** Protocol types and the frame codec: the head struct, the request
  reader (head line plus stdin), the frame writer and reader, round-trip and
  malformed-input tests. Needs: nothing.
- **G2 [gt]** The policy: `deploy/bd/verbs.json`, its loader, the class check, the
  refused flags, the env allowlist, and the test against
  `internal/cmdtree/bd-command-tree.json`. Includes producing the allowlist from
  the shim handlers and the daemon census. Needs: G1.
- **G3 [gt]** Authentication: the token directory loader, constant-time compare,
  class lookup, `SIGHUP` reload, and the 401 and 403 paths. Needs: G1.
- **G4 [gt]** The call log: the JSONL writer behind a buffered channel, rotation,
  the redaction rule (flag names and ids only), `/v1/stats` counters. Needs: G1.
- **G5 [gt]** The executor: path mapping from `rig` and `cwd` to the workspace,
  env construction, process-group start, stdin streaming with the cap, timeout
  and disconnect kill, exit status passthrough, the concurrency cap. Needs: G1.
- **G6 [gt]** `cmd/bdgw`: `serve` and `exec` modes wiring G2 to G5, `/healthz`,
  `/readyz`, a graceful shutdown, and an in-process end-to-end test with a fake
  bd. Needs: G2, G3, G4, G5.
- **G7 [gt]** The workspace store: `PUT /v1/workspace` validation (paths, file
  names, size), the atomic temp-tree swap, the manifest hash and the
  `X-Bd-Workspace` header. Needs: G1, P0.

G2, G3, G4, G5 and G7 touch different files and can run in parallel after G1.

## Phase 2: the gt side

- **G8 [gt]** `gt bd workspace push`: reads the town copy, builds the document,
  sends it; the `workspacePusher` interface called from `EnsureMetadata*` and
  `WriteRoutes` (nil by default); the daemon start and five-minute reconcile;
  `gt rig add` init through `bd.real` against a temp workspace. Two beads if it
  does not fit twenty minutes: G8a (push command and interface), G8b (daemon
  wiring and rig add). Needs: G7.
- **G9 [gt]** gt-wrap `bdgateway` target: the HTTP client, token file, env screen
  and town-relative translation of `BEADS_DIR` and the working directory, the
  frame reader, exit passthrough, the file-argument rewrite kept, `--doctor`
  lines. Needs: G1.
- **G10 [gt]** gt-wrap docker target for the bd container: `docker exec gt-bd bdgw
  exec`, the same translation, the doctor ladder for a stopped container (no
  throwaway rung for bd). Needs: G6, G9.
- **G11 [gt]** The forwarder as `bd` in the daemon image: install gt-wrap under the
  name `bd` ahead of the real bd on PATH, `BD_GATEWAY_URL` and the token file
  from the environment, with the fallback to the local bd when no gateway is
  configured. Needs: G9.

## Phase 3: images, compose, doctor

- **B1 [be]** beads repo `Dockerfile` and a make target building the linux/arm64
  bd with the town's flags and stamp; a shape test on the Dockerfile. Needs: nothing.
- **B2 [be]** beads repo `.forgejo/workflows/bd-image.yml` as reviewed text plus
  the test that holds a live copy equal to it. Needs: B1.
- **G12 [gt]** `deploy/bd/Dockerfile.bd` (FROM the base by digest from
  `deploy/bd/base-pin`, adds `bdgw` and the shim, `bd` becomes the shim and
  `bd.real` the real bd), and the daemonimage build step that cross-compiles
  `bdgw` and `bdshim` for it. Needs: G6, B1.
- **G13 [gt]** `imagevalues` gains the `bd:` key; `imagepublish` pushes and checks
  the third image; `gt update images` writes it; `gt update bd-base` resolves the
  base tag to a digest and writes the pin; `images.yml` reviewed text gains the
  bd-layer steps. Needs: G12.
- **G14 [gt]** `deploy/data/docker-compose.yml` (dolt and bd, limits, health
  checks, `depends_on`, ports, mounts) and a compose test in the style of
  `deploy/tracker/compose_test.go` (limits, loopback-only ports, no password in
  the file, health checks, the keep label, the image digest from the values
  file). Needs: G12.
- **G15 [gt]** `gt dolt start/stop/status` operate the compose unit; the old
  `ContainerPlan` path stays behind a flag; `gt doctor` check for both services,
  `readyz`, and the token file modes. Needs: G14.
- **G16 [gt]** The load measurement as a test-tier-excluded command
  (`cmd/bdgw-bench`): 200 `show` and 50 `list --json` through the gateway against
  the in-pod bd, printing p50, p95 and p99 against the budget. Needs: G6.

## Phase 4: callers, deletions

- **G17 [gt]** The daemon Deployment: `BD_GATEWAY_URL`, the token Secret mount,
  the render flag; golden manifests. Needs: G11, G13.
- **G18 [gt]** The dashboard Deployment the same way, class `read`. Needs: G11, G13.
- **G19 [gt]** Deletions: the daemon image's bd copy and the "build bd" step, the
  wrapper's kubernetes-target bd special cases, the `BEADS_DOLT_SERVER_HOST`
  rewrite, `ContainerPlan` and its flag, the `LookPath` probes turned into the
  readiness check; closes gt-q68hh2 and re-scopes gt-jqrnty. Needs: all of
  Stage 5 done and a week clean.

## Hand steps, in order

- **O1 [op]** Operator copies the two workflow texts and sets the beads repo
  secrets (needs B2, G13).
- **O2 [op]** Create the gateway tokens and the tracker add-as token, the Secret
  for the pods (needs G3 and gt-qr8pe).
- **O3 [op]** The Stage 1 start, the Stage 2 window (dispatch hold on, queue
  empty, backup taken), the Stage 3 to 5 flips (needs G10, G14, G15, G17, G18).

## Order and parallelism

1. Now, in parallel: P0, G1, B1 (beads rig).
2. After G1: G2, G3, G4, G5, G9 in parallel; after P0 and G1: G7.
3. After G2 to G5: G6. After G7: G8. After G6 and G9: G10. After G9: G11.
4. After G6 and B1: G12; then G13, G14; then G15; G16 any time after G6.
5. After G11 and G13: G17 and G18 in parallel.
6. Cutover (O3) when G10, G14, G15, G17, G18 are in; G19 a week later.

Beads-rig work is B1 and B2 only; everything else is gastown. B1 and B2 need the
beads rig unparked, or one crew session, and O1 needs the operator's hands for
the workflow file.

Overlap to watch: G2 and G5 both read the same env and path rules, so G2 lands
first or they share one small `internal/bdgateway/rules.go` file created in G1.
