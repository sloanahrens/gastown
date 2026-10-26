> Status: historical (2026-10). Merged in: gt-p85pw.61 (R7). Not maintained.

# R7: where gt creates, starts, stops and removes a seat, and the environment each kind gets

**Bead:** gt-p85pw.61 (epic gt-p85pw, track R, research). **Completed by:** polecat ace, 2026-10-26.
**Read at:** gastown `origin/main` d39fca8c5.
**Method:** read-only. No file changed outside this report; no fetch in a mayor/rig.
**Plan drift:** `docs/plans/2026-10-25-town-tracker-plan.md` lists R1-R5 only — it has no R7
entry, and plan task O6 (line 213) names `internal/agentconfig`, a package that does not
exist. The agent-config code is `internal/config` (`AgentEnv`, `ResolveDoltEndpoint`,
`ConfiguredDoltEnv`). Everything below is from the bead's own acceptance boxes, not the plan.

## Answer

gt has four seat shapes today, and only **two functions** create and start them:

- **Every polecat session**, host or sandbox, starts in `polecat.SessionManager.launch`
  (`internal/polecat/session_manager.go:633`). The session backend interface
  (`internal/session/backend.go:91`) decides whether the session runs on the host's tmux or
  inside an OpenShell sandbox; **k8s is the same backend with a different gateway driver**, and
  gt never talks to kubectl.
- **Every crew session** starts in `crew.Manager.Start` → `launchSession`
  (`internal/crew/manager.go:795`, `:984`). Crew has no sandbox backend: `StartOptions`
  (`internal/crew/manager.go:40`) has no `Backend` field.

Removal is equally concentrated: `polecat.Manager.removeWithOptionsLocked`
(`internal/polecat/manager.go:1584`) and `crew.Manager.Remove` (`internal/crew/manager.go:527`)
plus the `gt crew remove` command's kill (`internal/cmd/crew_lifecycle.go:41`). A sandbox is
deleted by `openshellBackend.Delete` (`internal/session/backend_openshell_ops.go:210`), reached
from session stop and from every rollback path.

So the tracker's token needs three injection points (polecat `launch`, crew env, sandbox
contract), one per-seat file, and three revoke points (polecat removal, crew removal, sandbox
delete). Section 5 is the numbered proposal.

## 1. Seat kinds and their lifecycle functions

| Kind | Create (filesystem / sandbox) | Start (session) | Stop | Remove |
|---|---|---|---|---|
| Host polecat (tmux) | `polecat.Manager.AllocateAndAdd` `internal/polecat/manager.go:922` → `addWithOptionsLocked` `:1081` (or `AddNamedWithOptions` `:1025`, `AddWithOptions` `:1243`) | `polecat.SessionManager.Start` `internal/polecat/session_manager.go:517` → `launch` `:633`; tmux backend `Prepare`/`Delete` `internal/session/backend.go:152`, `:174` | `supervisor.Supervisor.Kill` `internal/supervisor/supervisor.go:306` (sessions), `SessionManager.Stop` `internal/polecat/session_manager.go:935` | `polecat.Manager.RemoveWithOptions` `internal/polecat/manager.go:1506` → `removeWithOptionsLocked` `:1584`; `killSession` `:260` |
| OpenShell sandbox polecat (Docker driver) | `openshellBackend.Prepare` `internal/session/backend_openshell.go:350`: `sandbox.CLI.Create` `internal/sandbox/cli.go:142` called at `backend_openshell.go:436`, then `populate` `:458` (Ready, skeleton, clone, redirect, record) | same `SessionManager.launch` (`session_manager.go:633`); session made inside the sandbox through the routed tmux client (`backend_openshell.go:445`) | `openshellBackend.Stop` `internal/session/backend_openshell_ops.go:203` → `sandbox.CLI.Stop` `internal/sandbox/cli.go:301` | `openshellBackend.Delete` `:210` → `sandbox.CLI.Delete` `internal/sandbox/cli.go:314`; also rollback after a failed `populate` `:561`, and `SessionManager.removeSandbox` `internal/polecat/session_manager.go:975` → `session.CleanupSandboxWith` `internal/session/sandbox_hooks.go:31` |
| Kubernetes pod polecat | same `Prepare`; the **gateway** creates the pod from `driver_config_json` (`backend_openshell.go:606-612`, `kubernetesDriverConfig` `:832`). gt writes no manifest and runs no kubectl | same as sandbox | same as sandbox | same as sandbox |
| Crew (host tmux only) | `crew.Manager.Add` `internal/crew/manager.go:245` → `addLocked` `:258` | `crew.Manager.Start` `:795` → `launchSession` `:984` | `supervisor.StopSession` `internal/supervisor/supervisor.go:343`, called by `gt crew stop` (`internal/cmd/crew_lifecycle.go:665`) | `gt crew remove` `internal/cmd/crew_lifecycle.go:41` (kill at `:86`, directory at `:123`); `crew.Manager.Remove` `internal/crew/manager.go:527` |

There is no other seat kind. Deacon, witness and dog roles were deleted (see
`internal/daemon/supervisor_host.go:61-72`), so no other role reaches a session start.

### Entry points that reach those functions

| Entry point | Where | What it does |
|---|---|---|
| `gt sling` (new polecat) | `internal/cmd/polecat_spawn.go:738` allocates, `:1275` resolves the backend, `:1281` starts | create → start with the seat's backend |
| Seat restart (dispatcher, `gt up`, `gt session restart`) | `internal/cmd/pool_seats.go:408` and `:426`, `internal/cmd/session.go:305`, `:711`, `internal/cmd/up.go:722` | start only; the worktree and sandbox are rebuilt by `Prepare` |
| Daemon restart (stalled seat) | `internal/daemon/patrol_scan.go:949` shells out to `gt session restart --force` | start only |
| `gt polecat nuke` | `internal/cmd/polecat.go:3388` | remove (`Nuclear`) |
| `gt shutdown` cleanup | `internal/cmd/start.go:474` | remove |
| Daemon idle reaper | `internal/daemon/daemon.go:3578` `sup().Kill` | stop only (worktree survives) |
| Patrol stalled-seat kill | `internal/daemon/patrol_scan.go:723` | stop, then sandbox delete (`:726`) |
| `gt crew remove` | `internal/cmd/crew_lifecycle.go:86`, `:123` | stop, then remove directory |
| `gt up` | `internal/cmd/up.go:616` (crew start) | start only |

## 2. Environment each kind receives

The single source for role environment is `config.AgentEnv`
(`internal/config/env.go:147`). It is called by the polecat session manager
(`session_manager.go:693`), by the crew manager (`crew/manager.go:853`), and by the startup
command builders (`internal/config/loader.go:2450`, `:2482`).

| Variable | Host polecat | Crew | Sandbox polecat |
|---|---|---|---|
| `GT_ROLE` | `<rig>/polecats/<name>` `env.go:156` | `<rig>/crew/<name>` `env.go:177` | same address, set again by the sandbox contract `backend_openshell.go:641` |
| `GT_RIG`, `GT_POLECAT` / `GT_CREW` | `env.go:157-158` | `env.go:178-179` | `GT_RIG`/`GT_POLECAT` `:639-640` |
| `BD_ACTOR` | `<rig>/polecats/<name>` `env.go:159` | `<rig>/crew/<name>` `env.go:180` | same actor `backend_openshell.go:642` |
| `BEADS_AGENT_NAME` | `<rig>/<name>` `env.go:200` | same `env.go:200` | inherited from the spawn env (not overridden) |
| `BEADS_DOLT_SERVER_HOST` / `_PORT`, `BEADS_DOLT_PORT` | the town's endpoint from `mayor/town.json`, `ConfiguredDoltEnv` `env.go:465` (keys at `:460`) | same | rewritten by the contract: host = control-plane host, port = `"3307"` `backend_openshell.go:650-651` |
| `BEADS_DOLT_SERVER_MODE` | not set | not set | `"1"` `backend_openshell.go:649` |
| `BEADS_DOLT_AUTO_START` | `"0"` `env.go:288` | `"0"` | inherited host value (not dropped) |
| `BD_DOLT_AUTO_COMMIT` | `"off"` `env.go:165` | not set | inherited |
| `GT_TOWN_ROOT`, `GT_ROOT` | town root `env.go:188-191` | same | `/sandbox/gt` `backend_openshell.go:637-638` |
| `GIT_CEILING_DIRECTORIES` | town root `env.go:195` | same | rewritten from the host town root to `/sandbox/gt` by `rewriteTownRootInPathEnv` (`internal/polecat/sandbox_paths.go:33`, called `session_manager.go:795`) |
| `GOFLAGS` (`-p` cap) | `env.go:174` | not set | inherited (not dropped) |
| `GT_SESSION`, `GT_AGENT`, `GT_PANE_ID` | `env.go:216`, `:226`; `GT_PANE_ID` `session_manager.go:832` | `session.MergeRuntimeLivenessEnv` `crew/manager.go:861`; `GT_PANE_ID` `:1001` | `GT_SESSION_BACKEND=openshell`, `GT_TMUX_SOCKET` `backend_openshell.go:643`, `:648` |
| Provider / model passthrough | `ProviderPassthroughEnv` `env.go:380` | same | inherited; `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` dropped `backend_openshell.go:46` |
| Files | worktree under `<rig>/polecats/<name>/<rig>`; `.beads/redirect` (`Manager.setupSharedBeads` `internal/polecat/manager.go:3992` → `beads.SetupRedirect` `internal/beads/beads_redirect.go:341`), `CLAUDE.md` `:1174`, `PRIME.md` `:1183`, overlay `:1187`, `.claude/settings.json` `:1196`; agent bead `:1209` | clone under `<rig>/crew/<name>`, `.beads/redirect` (`crew/manager.go:338` → `:783`), mail dir `:331`, settings `:376-380`, overlay `:353` | town skeleton (`internal/sandbox/skeleton.go:426`: `mayor/town.json`, `.beads/routes.jsonl`, rig metadata, rig `.beads/config.yaml`, both `metadata.json` with `dolt_server_host` `:516`, scrubbed `settings/config.json` `:532`, directives, bare repo) extracted at `/sandbox` `backend_openshell.go:468-479`; runtime archive (`internal/session/sandbox_runtime.go:56-90`: `.claude.json` 0600, polecat `settings.json`, git helper, `.gitconfig`); the in-sandbox clone `:485`; `.beads/redirect` `:501` |

How the environment is delivered:

- **Host polecat and crew**: twice. As tmux session environment (`-e`, set before the pane
  starts) through `tmux.NewSessionWithCommandAndEnv` (`internal/tmux/tmux.go:571`, called at
  `session_manager.go:824` and `crew/manager.go:995`), and as an `export … &&` prefix on the
  startup command (`config.ExportPrefix`, applied at `internal/config/loader.go:2070` and `:2359`).
- **Secrets by reference** already exist: an agent env value written as `${VAR}` is not expanded
  into the argv; the startup command reads it from `settings/daemon.env` (0600) when it runs
  (`internal/config/secrets.go:11-34`, `resolveSpawnEnv` `:237`, `mergeAgentEnv` `:280`,
  `daemonEnvShellWord` `:308`, `DaemonEnvPath` `internal/config/daemon_env.go:18`). The prefix is
  omitted for a sandbox (`OmitSecretPrefix`, `loader.go:2301`, set from
  `Prepared.OmitSecretPrefix` `backend_openshell.go:446`), because a sandbox has no such file.
- **Sandbox**: `sandbox create --env-from KEY=GT_SANDBOX_ENV_KEY` with the value in the child
  process environment, never the argv (`internal/sandbox/cli.go:142`, `appendEnvFrom` `:355`);
  the agent's own startup command gets `Prepared.Env` laid over the host env
  (`openshellBackend.agentEnv` `backend_openshell.go:662`, applied at `session_manager.go:801-809`).

## 3. Port 3307 and the Dolt host from a sandbox

| Place | file:line | What it pins | Matters for the tracker? |
|---|---|---|---|
| Sandbox Dolt port constant | `internal/session/backend_openshell.go:36`, used `:651` | `sandboxDoltPort = "3307"` in every sandbox's contract | yes — the constant a `TRACKER_PORT`/`TRACKER_URL` sits beside |
| Default control-plane host | `internal/session/backend_openshell.go:37`, `:686`; `internal/config/types.go:1729`, `:1740` | `host.docker.internal` unless the profile sets `control_plane_host` | yes — the host a sandbox URL must name |
| Sandbox metadata | `internal/sandbox/skeleton.go:516` | writes `dolt_server_host` = control-plane host into both `metadata.json` files | yes — an existing per-seat endpoint write to copy |
| Town endpoint env | `internal/config/env.go:460`, `:465`, resolver `:407` | `BEADS_DOLT_SERVER_HOST/_PORT`/`BEADS_DOLT_PORT` from `mayor/town.json`; no 3307 fallback | yes — the tracker endpoint should resolve the same way |
| Dolt default port | `internal/doltserver/doltserver.go:151` | `DefaultPort = 3307`, used by `gt install`/`gt dolt start` | no (host side), but the tracker's default should be its own constant |
| `gt install --dolt-port` default | `internal/cmd/install.go:91`, endpoint written `:253-256` | 3307 when the flag is unset | pattern for `tracker.port` at install |
| Config help text | `internal/cmd/config.go:678` | documents `dolt.port` | doc to update |
| `gt power status` label | `internal/power/states.go:31` | prints `dolt :3307` (label text, not a connection) | yes — the tracker wants its own line |
| hoststat process matcher | `internal/hoststat/hoststat.go:302` | `productionDoltPort = "3307"` finds the town's Dolt process | no (host process sampling) |
| Test-only constant | `internal/beads/test_container.go:236` | test firewall | no |
| Docker sandbox policy | `deploy/polecat/policy.yaml:75-78` | `dolt` egress to `host.docker.internal:3307`, binaries `bd`/`gt` | **yes** — a tracker port needs its own endpoint block and binaries |
| kind sandbox policy | `deploy/k8s/policy/kind.yaml:76-80` | `dolt` egress to `control-plane.openshell.svc.cluster.local:3307`, pinned `allowed_ips` | **yes** |
| Policy renderer | `deploy/k8s/render-policy.sh:20`, `:87` | rewrites `host.docker.internal` to the in-cluster name; port `3307` → `dolt`, `3000` → `forgejo` | **yes** — a tracker port needs a third name |
| k8s control-plane names | `deploy/k8s/services/control-plane.yaml:13-16` | ExternalName `dolt` (and `control-plane`) → `host.lima.internal` | **yes** — a `tracker` name belongs here |

**The sandbox's path to the host is a host alias, not a port forward.** Docker: the profile's
`control_plane_host`, default `host.docker.internal` (`backend_openshell.go:686-691`), reaches the
Mac's service. kind/Kubernetes: an ExternalName Service (`dolt`, `control-plane`) that resolves to
`host.lima.internal`, with the policy pinning the address the name resolves to
(`deploy/k8s/services/control-plane.yaml:1-16`, `deploy/k8s/policy/kind.yaml:78-80`,
`deploy/k8s/render-policy.sh:80-100`). The design note is explicit that the gateway is reached
through a kind port mapping and a NodePort Service, **not** `kubectl port-forward`
(`docs/design/k8s-sandboxes.md:14`). The tracker plan's phrase "the same port forward as 3307"
therefore means "the same host alias / ExternalName route", and trackerd bound to `127.0.0.1`
(`tracker/server/server.go:186-194`) rides it exactly as Dolt does — Dolt's default bind is
`127.0.0.1` too (`doltserver.go:447-462`), and that path is the one verified on kind on
2026-10-24 (`deploy/k8s/render-policy.sh:1-13`). Re-verify it for trackerd rather than assume it.

## 4. What the tracker client already expects

- `tracker/client.FromEnv` reads `TRACKER_URL` and either `TRACKER_TOKEN_FILE` or
  `TRACKER_TOKEN` (`tracker/client/client.go:59`), and `internal/beads/backend.FromEnv` is the
  gt-side seam (`internal/beads/backend/backend.go:38`); a rig routed to the tracker with no URL
  or no token fails closed (`:80-86`). Note: `backend.Open` is landed but **no production caller
  imports it yet** (grep finds only the package itself and a comment in
  `internal/beads/routes.go:22`), so the env contract is fixed and the call site is still owed.
- `client.New` refuses plain `http` to anything but a **literal loopback IP**
  (`tracker/client/client.go:152-166`). A host polecat's `http://127.0.0.1:<port>` passes; a
  sandbox's `http://host.docker.internal:<port>` or
  `http://control-plane.openshell.svc.cluster.local:<port>` does **not**. This is the one
  decision a sandbox token cannot avoid: either the client learns the profile's control-plane
  host, or trackerd gets TLS (the design already says TLS/mTLS arrives when it leaves the host,
  `docs/plans/2026-10-25-town-tracker-design.md:150`).
- Tokens are stored hashed (`tracker/store/tokens.go:21-24`), issued with
  `IssueTokenScoped` (`:45`), revoked with `RevokeToken` (`:91`). The only issue path today is
  the operator CLI `trackerd -issue-token ACTOR -token-file F`, which writes the token to a 0600
  file and never prints it (`tracker/cmd/trackerd/run.go:121-139`, usage
  `tracker/cmd/trackerd/main.go:5`). `api.ScopeAdmin` is defined to allow issuing tokens
  (`tracker/api/actor.go:26`), but **no HTTP route issues or revokes one** — the admin routes are
  stats and config only (`tracker/server/routes_admin.go:14-18`).
- Seat identity is already a first-class string: `intent.Seat` renders `<rig>/<role>/<name>` and
  its record lives at `<town>/.runtime/agents/<rig>/<role>.<name>.json`
  (`internal/intent/intent.go:71-98`), removed with the polecat in
  `internal/polecat/manager.go:1751`. That directory is the natural home for a per-seat token file, and the token's
  actor string is the same address as `GT_ROLE`/`BD_ACTOR`.

## 5. Proposal: where to issue, store, inject and revoke

Each item is one small change. Ordering follows the dependency: (1)-(3) are the service and
config groundwork, (4)-(8) the seat wiring, (9)-(12) cleanup and the k8s route.

1. **Endpoint config.** Add `tracker` (`{host, port}`) beside `dolt` in `mayor/town.json`
   (`internal/config/types.go:43-48` is the shape to copy) and a `ResolveTrackerEndpoint` that
   reads durable config only, like `ResolveDoltEndpoint` (`internal/config/env.go:407`). Have
   `gt install` write it (`internal/cmd/install.go:253-256`) and `gt config set tracker.port`
   write it too. No code should construct `http://127.0.0.1:7433` by hand.
2. **An admin credential for gt.** Put `TRACKER_ADMIN_TOKEN=${…}` in `settings/daemon.env`
   (0600) and read it through the existing reference mechanism
   (`internal/config/secrets.go:237-308`) in the host processes that issue and revoke: the
   sling/spawn path, `gt session restart`, `gt crew remove`, `gt polecat nuke`. It never goes
   into a seat's environment and never appears in argv.
3. **Admin HTTP routes.** Add `POST /v1/tokens` (issue: actor + scope, token returned once) and
   `POST /v1/tokens:revoke` behind `requireAdmin` (`tracker/server/routes_admin.go:18` shows the
   pattern), so gt never holds the Postgres DSN and the "clients never connect to Postgres" rule
   (`docs/plans/2026-10-25-town-tracker-design.md:143`) holds. The alternative — shelling out to
   `trackerd -issue-token` — needs the DSN in gt's environment; prefer the routes.
4. **Per-seat token file.** `<town>/.runtime/seats/<rig>/<role>.<name>.token` (directory 0700,
   file 0600, created exclusively like `tracker/cmd/trackerd/run.go:122`). It sits beside the
   intent record (`.runtime/agents/...`) and, unlike `settings/`, is never shipped into a
   sandbox by the skeleton (`internal/sandbox/skeleton.go:426-560` ships only the named files).
5. **Issue on polecat start.** In `polecat.SessionManager.launch`, after the session ID is known
   and before the backend `Prepare` (`internal/polecat/session_manager.go:693-742`): if the
   seat's token file is absent, issue one for actor `<rig>/polecats/<name>` and write it; if
   present, reuse it. Restarts (dispatcher, daemon, `gt up`) then keep the same token, which is
   what "one per seat" means, and the sandbox path gets a token before `Prepare` stages files.
6. **Inject for a host polecat.** Add `TRACKER_URL` and `TRACKER_TOKEN_FILE` to `envVars` in the
   same block (`session_manager.go:693-712`), so both the tmux `-e` env and the export prefix
   carry them. `TRACKER_TOKEN_FILE` names the file from (4); the token itself never enters argv
   (the G3-18 rule the daemon.env mechanism already follows).
7. **Inject for crew.** Same two variables in the crew env block
   (`internal/crew/manager.go:853-861`), with the file written by `crew.Manager.Start` just after
   the worker is resolved (`:811-830`).
8. **Inject for a sandbox.** In `openshellBackend.contract` (`backend_openshell.go:634-657`) add
   `TRACKER_URL` built from `b.controlPlaneHost()` and the tracker port, and
   `TRACKER_TOKEN_FILE=/sandbox/.tracker/token`; stage the token file with mode 0600 through the
   runtime archive (`internal/session/sandbox_runtime.go:56-90`, extracted at
   `internal/session/backend_openshell.go:474-479`). Do
   **not** put it in `settings/config.json`: the skeleton scrubs credentials there
   (`ScrubSettings` `internal/sandbox/skeleton.go:136`, used `:527-532`). The environment
   carriage (`--env-from`, `cli.go:355`) is the design's fallback but a file is stricter and the
   archive already carries 0600 files.
9. **Let a sandbox URL pass validation.** Either teach `checkBaseURL`
   (`tracker/client/client.go:152-166`) to accept plain http to the same control-plane host the
   store contract uses, or put TLS in front of trackerd for sandbox callers. Decide this in the
   bead that wires (8); it is the only item here that changes tracker code rather than gt code.
10. **Revoke on polecat removal.** `removeWithOptionsLocked`
    (`internal/polecat/manager.go:1584-1743`), next to `clearIntentRecord` `:1751` — that is the
    one place `gt polecat nuke` (`internal/cmd/polecat.go:3388`), `gt shutdown`
    (`internal/cmd/start.go:474`) and the spawn rollbacks (`polecat_spawn.go:764-768`) all reach.
    Revoke with the admin credential from (2); a failed revoke is a warning, never a refusal to
    remove work.
11. **Revoke on sandbox delete and on crew removal.** `SessionManager.removeSandbox`
    (`session_manager.go:975-991`) and `openshellBackend.Delete`
    (`backend_openshell_ops.go:210-225`) cover stop, patrol cleanup and the failed-`populate`
    rollback `:561`; revoke once, idempotently, in `Delete` (a sandbox that is already gone counts
    as deleted). For crew, revoke in `gt crew remove` after the workspace is gone
    (`internal/cmd/crew_lifecycle.go:123-135`). Do **not** revoke on stop or on the idle reaper
    (`internal/daemon/daemon.go:3578`): the worktree survives and the seat may be restarted with
    the same token, which is the design's rule — revoked at removal.
12. **k8s route.** Add a `tracker` ExternalName Service beside `dolt`
    (`deploy/k8s/services/control-plane.yaml:13-16`), a tracker endpoint block in the rendered
    policy (`deploy/k8s/policy/kind.yaml:76-80` via `render-policy.sh:80-100`, which needs a third
    port→name mapping at `:87`), and the same endpoint in the Docker policy
    (`deploy/polecat/policy.yaml:75-78`) with `gt` and `bd` in its `binaries` list. Without the
    policy block the sandbox's TCP connect to the tracker is denied, exactly as it would be for
    Dolt.

## 6. Gaps found while reading

- The plan has no R7 task, and its O6 entry names `internal/agentconfig`, which is not a package.
  Whoever builds O6 should read `internal/config/env.go` instead, and this report's section 3.
- `internal/beads/backend.Open` has no production caller yet, so "a rig on the tracker" is still
  a route-table field plus an unused opener as of d39fca8c5.
- `gt power status` prints the literal `dolt :3307` (`internal/power/states.go:31`); a tracker
  line has to be added there rather than reusing that label.
- Nothing in gt or the policies carries a tracker port today; every 3307 listed in section 3 is a
  place the tracker's port must be added, and no place exists to copy from except `dolt` itself.
