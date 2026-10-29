> Status: historical (2026-10). Merged in: this bead. Not maintained.

# Does `internal/session` call Docker on the daemon path?

Task R3 of the daemon-container plan (`docs/plans/2026-10-28-daemon-container-plan.md`), bead gt-qpeei.3. Read from `origin/main` at `5739f9b8`. No code changed, and nothing was run: every finding is a static reading of the code, so each is a hypothesis until a run confirms it.

## Answer

With the OpenShell backend and the kubernetes driver (`sandboxes.<name>.driver: kubernetes`):

| Daemon-reached path | Runs `docker`? | Why |
|---|---|---|
| Spawn (sandbox `Prepare`) | **yes** | `Prepare` resolves and inspects the image through the host's docker on every driver. Three `docker image inspect` calls on a tag, two on a digest-pinned image. |
| Nudge | **no** | Routed through `openshell sandbox exec` (`SandboxExecPrefix`). |
| End (stop, push, delete) | **no** | Routed through `openshell sandbox exec` for the push and `openshell sandbox delete` for the removal. |

The spawn yes is the one that matters for the container program: the kubernetes driver still needs a docker CLI and a reachable docker daemon on the host where `Prepare` runs.

## Every `docker` reference in `internal/session`

All non-test references are in `internal/session/backend_openshell.go`. `backend.go` and `sandbox_registry.go` mention Docker only in comments and config constants.

| Line | What it does | Condition that reaches it |
|---|---|---|
| `:173-175` | Wires `imageGT: dockerImageGT`, `imageRef: dockerImageRef`, `docker: runDocker` in `newOpenShellBackend` | Always, for every openshell seat. The driver does not change the wiring. |
| `:195-196` | `dockerImageGT` calls `imageGTFrom` with `runDocker` | Called from `Prepare` at `:663`, for every driver. |
| `:200-201` | `runDocker` runs `exec.CommandContext(ctx, "docker", args...)` | The only place the `docker` binary is executed in this package. |
| `:211-213` | `imageGTFrom` runs `docker image inspect --format <gt-commit label> <image>` | Reached from `dockerImageGT` (`:663`), every driver. |
| `:241-242` | `dockerImageRef` calls `imageRefFor` with `runDocker` | Called from `Prepare` at `:653`, every driver. |
| `:263-281` | `imageRefFor` calls `imageIDFrom` (`:264`) and, when `pull` is set and the image is not digest-pinned, `imageRepoDigestsFrom` (`:274`) | `pull` is `driverPullsImage(profile.Driver)` (`:238`), which is true only for `kubernetes`. |
| `:286-287` | `imageIDFrom` runs `docker image inspect --format {{.Id}} <image>` | Always, via `imageRefFor` (`:264`). |
| `:303-304` | `imageRepoDigestsFrom` runs `docker image inspect --format {{json .RepoDigests}} <image>` | Kubernetes driver, unless the image is already a digest reference (`digestRefPattern`, checked at `:268`, after the id call at `:264`). |
| `:674-675` | `ensureCacheVolume` on the docker cache volume | Only when `usesDockerVolumes()` (`:917-928`): a `cache_volume` is set and the driver is `""` or `docker`. Never for kubernetes. |
| `:1059-1078` | `ensureCacheVolume` runs `docker volume inspect` (`:1066`) and `docker volume create` (`:1073`) | Same condition as `:674`. |
| `:896-901` | `createSpec` builds the cache mount as gateway config JSON (`cacheDriverConfig`, `:1193-1200`) | Docker driver only. A config value handed to the gateway, not a `docker` argv: the gateway, not this host, mounts it. |

`Prepare` calls them in this order, so the spawn path's docker calls all happen before any push or sandbox create:

- `:653` `imageRef` → `:263` `imageRefFor` → `:286` id (always), `:304` repo digests (kubernetes, tag only).
- `:663` `imageGT` → `:213` label (always).
- `:674` cache volume (docker driver with a cache volume only).

## Spawn path (yes)

Reached from the daemon through two subprocess routes; the daemon does not call the session package directly for spawn.

1. **Patrol restart.** `internal/daemon/patrol_scan.go:1053` runs `gt session restart <rig>/<name> --force`. `internal/cmd/session.go` `runSessionRestart` then calls `restartPolecatOnSlungAgent` (`internal/cmd/pool_seats.go`), which calls `townSeatBackend` → `sessionBackendForAgent` (`internal/cmd/session_backend.go`) → `session.Resolve`. The resolved openshell backend reaches `SessionManager.Start` → `launch`, which calls `Prepare` at `internal/polecat/session_manager.go:752`.
2. **Sling.** `internal/daemon/scheduled_slings.go:320` (`gt sling`) and `internal/daemon/spec_slings.go:221` (`gt spec sling --job`) run the sling CLI. Its spawn chooses the backend at `internal/cmd/polecat_spawn.go:1255` (`sessionBackendForAgent`) and passes it on at `:1264`. I did not trace the rest of this route down to `Prepare`; route 1 is the one traced end to end.

The config that selects it:

- The seat: `polecat_pool.seats` entry with `backend: openshell` (`internal/cmd/session_backend.go:36`, `seat.Backend == config.BackendOpenShell`).
- The driver: `sandboxes.<name>.driver: kubernetes` (`internal/config/types.go:1709`). `WithDefaults` (`types.go:1763`) fills `docker` when unset, and the value is copied into the profile at `internal/cmd/session_backend.go:49`.
- The gate that does not help: `Prepare` calls `imageRef` and `imageGT` unconditionally (`:653`, `:663`), so a kubernetes driver never skips the docker image reads. Only the cache volume (`:674`) and the pull mode (`:238`) depend on the driver.

## Nudge path (no)

The daemon nudges through its tmux client (`internal/daemon/tmux_seam.go:27`, `NudgeSession`). For a sandboxed seat that client is routed by `sandboxRouting.route` (`internal/session/sandbox_routing.go:58-100`), which returns `tmux.NewTmuxWithExecPrefix(SandboxTmuxSocket, SandboxExecPrefix(rec.Gateway, session))`. `SandboxExecPrefix` (`internal/session/sandbox_registry.go:133-139`) is `openshell [-g gateway] sandbox exec -n <session> --no-tty --no-login-shell --`. No docker argv.

The overrun guard's nudge (`internal/daemon/overrun_guard.go:573`) goes through `notify().Nudge`. `internal/nudge` has no docker reference, and I did not trace its tmux call further.

## End path (no)

Two daemon routes end a sandboxed seat:

1. **Overrun stop.** `internal/daemon/overrun_guard_stop.go:936-945` calls `polecat.NewSessionManager(...).Stop(seat, false)` with no `SetHooks`. `SessionManager.Stop` (`internal/polecat/session_manager.go:946`) kills the session through the routed tmux client (`KillSessionWithProcesses`), then `removeSandbox`. `removeSandbox` runs `SandboxPusher` and `SandboxDeleter`.
2. **Patrol cleanup.** `internal/daemon/patrol_scan.go:873` calls `session.CleanupSandboxWith(session.SandboxDeleter, ...)`.

The two seams resolve to:

- `SandboxPusher` = `pushSandboxRecord` → `pushWork` (`internal/session/backend_openshell_ops.go:31`, `:43-66`): `git push` inside the sandbox through `openshell sandbox exec`. No docker.
- `SandboxDeleter` = `deleteSandboxRecord` → `deleteSandbox` (`backend_openshell_ops.go:25-26`, `:299-306`): `openshell sandbox delete`. No docker.

`openshellBackend.Stop` and `Delete` (`backend_openshell_ops.go:283-296`) call `openshell` through `sandbox.CLI`; `Delete` also removes the local registry record. `internal/sandbox` has one exec site, `cli.go:381`, which runs `openshell` (`cli.go:71`). No docker.

## What this means for the container program

- Within `internal/session`, the nudge and end paths on the kubernetes driver need no docker CLI or socket. The daemon's own docker calls outside this package (see the last section) still need one.
- A spawn on the kubernetes driver needs a `docker` binary on the host where `Prepare` runs, plus access to the host's docker image store. Without them, `Prepare` fails at `:653` (`docker image inspect`) with the error from `:289`, before any push or create. That failure is a clean refusal, not a silent fallback.
- Changing this would mean a new image resolver for the kubernetes driver. That is a design decision for the overseer, not part of R3.

## Not covered

- Runtime. No run was made, so the spawn yes is a static finding. A run that reaches `Prepare` with the kubernetes driver and a docker-less `PATH` would confirm it: expect `exec: "docker": executable file not found` from `runDocker`.
- The `sling` route from `polecat_spawn.go:1255` to `Prepare` was read only at its entry point.
- The retired Docker-driver path itself (out of scope per the bead).

## Outside `internal/session`, seen while tracing

These are docker calls on daemon-reachable code that the bead does not cover. I did not trace their full reachability.

- `internal/daemon/attention_host.go:137` runs `docker ps --format {{.Names}}` through `systemHostProbes.Containers`. The only caller is `attention_host.go:227`, so it appears to run on the daemon's attention tick whatever backend a seat uses.
- `internal/daemon/disk_maintenance.go:401` builds a registry target whose GC runs `docker exec` (`internal/diskmaint/registry/gc.go:56`). The daemon reaches the target; whether a tick runs the GC is not traced.

Both belong in R4's or a follow-up bead's scope, not in this report's answer.
