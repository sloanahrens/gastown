> Status: historical (2026-10). Merged in: gt-1e1sh. Not maintained.

Approved by the operator 2026-10-28 (grilling rounds 1-3 and design sections 1-5, recorded on bead gt-1e1sh and the operator's handoff bead). The live work is the beads filed from the plan, `2026-10-28-daemon-container-plan.md`. Author: seagull.

# The gastown daemon in a container (interim, on this Mac)

## 1. Why, and what this is not

The always-on control plane will eventually run in a control cluster (map claude-2or, paused). That needs `gt daemon run` to run in a container, and nobody has done it. The Dockerfile, compose file and entrypoint at the repo root are an interactive dev sandbox (`sleep infinity`, builds `gt` inside, runs `gt install /gt`; `docs/docker.md`), not a service.

This program runs the live daemon in a container on this Mac, in Docker Desktop's VM beside Forgejo, behind a proof ladder. It is not a move to Kubernetes, not a cloud step, and not a change to how polecats run (sandbox seats stay on the OpenShell k8s gateway). Dolt, Forgejo, the OpenShell gateway and the dashboard stay where they are.

## 2. Decisions (approved)

1. Prove it in a scratch town first, never the live daemon.
2. The ladder: L1 the daemon starts and the patrol watchdog stays green in the scratch town; L2 it dispatches a bead and lands it through a throwaway Forgejo and runner; L3 the live launchd daemon is replaced by the container in a window the operator chooses, with launchd kept as the rollback.
3. `gt` gets a third supervisor kind, `container`, so `gt daemon status`, `stop`, `restart` and the install and upgrade flow work against the container.
4. Build and deploy (`rebuild_gt`, `rebuild_om`, the polecat-image build, the tool-updater, the installer) stay host-side in a Go deployer that extends the `internal/polecatimage` program pattern. The daemon's own `rebuild_*` patrols are off in container mode. A new image replaces the container.
5. The container runs in Docker Desktop's VM with no `docker.sock` mount.
6. `om`'s credential is decided before L3. The scratch proof runs with `landing_worker.review=false`.
7. Go, not scripts: the entrypoint (a `gt` subcommand), the supervisor kind, the deployer and the host agent are Go. The Dockerfile and compose file are config. No new shell.
8. The tier sweep gets its own docker-in-docker engine; the runner-idle command, wait and poll are deleted.
9. A Go host agent writes host health to a file the container daemon reads.
10. The container is the daemon's identity, not a pid.
11. The scratch town lives in its own directory (a field, never the live town root) with its own Dolt; L2 adds a throwaway Forgejo and runner; L3 bind-mounts the live town root at the same absolute path.

## 3. Facts the design rests on

Read from `origin/main` (1ba55cf06) on 2026-10-28.

- The daemon is one process, `gt daemon run`, under launchd (`com.gastown.daemon`), with 35 patrols (31 enabled). It has no socket or API: the pid file, `daemon.lock` and `daemon/state.json` are how other processes see it.
- `daemon.IsRunning` treats the `daemon.lock` flock as authoritative (`internal/daemon/daemon.go`). A flock held by a container is not visible to a Mac process across a Docker Desktop bind mount, so a Mac-side `gt daemon status` would say "not running" and a second daemon could start on the same town root. Section 5 closes that.
- The heartbeat is `daemon/state.json` (`last_heartbeat`, written every `HeartbeatInterval`, 5 minutes by default). `.runtime/daemon-liveness.json` is the doctor check's baseline, not a heartbeat.
- `internal/cmd/daemon_supervisor.go` models a supervisor as argv for start, stop and bootstrap plus a state reader (`templates.SupervisorReader`); `gt-3jrm` and `gt-uctd` made `daemon start` go through the provisioned supervisor and refuse while a manual daemon holds the lock.
- The daemon reads the host through the `hostProbes` interface in `internal/daemon/attention_host.go` (disk, free memory, running containers, rig parked). Production runs `memory_pressure` and `docker ps`. `internal/daemon/pressure_darwin.go` uses `vm_stat`; `burst_sampler.go` writes `metrics/bursts.jsonl` from macOS `top`.
- `internal/daemon/tier_sweep.go` waits up to `runner_idle_wait` for the CI runner to be idle by running `runner_idle_command` (`docker exec forgejo-docker-in-docker-1 ...`), because on 2026-10-08 the sweep and the CI gates shared one Docker engine and the engine stalled (`gt-5ejux`). The sweep finds real problems: 39 `tier-sweep-red` beads since 10-14.
- `doltserver` has an `IsRemote` mode: a non-loopback Dolt host means the daemon does not start Dolt. `town.json` `dolt.host` and `dolt.port` are the single source for the endpoint, and `gt` does not read it from the environment (`gt-y3pgh.3`).
- The polecat image flow already cross-compiles `gt` and `bd` for linux/arm64.
- External tools in daemon-reachable packages: `git`, `dolt`, `bd`, `om`, `go`, `make`, `ps`, `sysctl`, `memory_pressure`, `vm_stat`, `launchctl`, `tmux`, `tar`, `docker`, `openshell`, `crane`, `codesign`, `brew`.

## 4. Shape

- One image built from a Dockerfile that only copies in the deployer's Linux arm64 `gt`, `bd` and `om`, the `dolt` client, `git`, `tmux` and CA certificates. Nothing is built inside the image. The entrypoint is a `gt` subcommand.
- One compose project per town: the daemon container (`DOCKER_HOST=tcp://sweep-dind:2375`, no published ports, no Docker socket) and the `sweep-dind` engine on an internal network. The scratch project adds a Dolt container, and for L2 a throwaway Forgejo and runner pair.
- Host side: the deployer builds the binaries and the image and replaces the container; the host agent writes the probe file; launchd keeps running the live daemon until L3.

## 5. The container supervisor and the takeover guard

**Choosing the kind.** GOOS cannot pick it, so a town setting `daemon.supervisor` (`launchd`, `systemd`, `container`, unset for today's detection) selects it, and `detectDaemonSupervisor` gets a third case. Everything goes through the existing argv seam, so unit tests use fakes:

- start: `docker compose -p gt-town -f <rendered file> up -d daemon` (it also recreates the container when the image tag changes);
- stop: `docker compose ... stop daemon` (a manually stopped container stays stopped under `restart: unless-stopped`);
- state: `docker inspect` gives status and image; the status line reads `Supervised: container <name> running <image>`.

The compose file's `stop_grace_period` must be at least the daemon's `ShutdownBudget`, enforced by a test as the plist's `ExitTimeOut` is (`gt-x872`).

**Who owns the town.** Every `gt daemon run`, whatever started it, writes an owner into `state.json`: `container` (id: the one container name the compose file, the `docker inspect` target and the owner all share), `launchd` (id: the job label launchd reports in `XPC_SERVICE_NAME`), `systemd` (id: the invocation id) or `manual` (id: host and pid). State written by an older binary has no owner and is treated as the legacy case below. Because the id is the container's name, a recreate keeps the same owner id, and a container that no longer exists is provably not running, so a recreate never leaves an owner nobody can check.

**Inside the container.** Compose sets an environment variable that puts `gt daemon run` in container mode, in which it writes no pid file, because a pid from another namespace means nothing on the Mac. Every Mac-side reader that signals by pid (`StopDaemon`, `gt down`, and the four doctor daemon checks) refuses when the supervisor kind is container, each with a test that fails if the refusal is removed, and each doctor check has a test of its failing branch (container not running, heartbeat stale, state unknown). The plan's first audit lists every such call site.

**Takeover guard.** The guard runs in the daemon's own startup, at the top of `Run`, because that is the one place every start passes through: launchd and systemd run `gt daemon run` themselves and the container's entrypoint execs it, so a guard only in the CLI start commands would miss them. The CLI start paths call the same function first so the operator is refused before anything is launched. It is a pure function `CheckTakeover` over what the caller read: `state.json` (nil when missing), whether any other sign of a prior daemon exists (`daemon.lock` or `daemon.log`), the owner kinds this town could have, the clock and the caller. The rules, in order: no `state.json` and no other sign of a prior daemon allows the start; a missing `state.json` or an ownerless legacy one with a leftover lock or log refuses, because a container's lock is not visible across the mount and the file may simply have been deleted; the caller being the recorded owner allows; a recorded clean stop (`running: false`) allows when the previous owner is shown not running, whatever the heartbeat age, which is the clean handoff the cutover uses; a heartbeat under 12 minutes old (more than twice the interval) refuses; and a stale heartbeat allows only when the owner is shown not running, because a stale heartbeat can be a stuck daemon. `--takeover` (the environment variable `GT_DAEMON_TAKEOVER=1` that the compose file passes through for one start) overrides a refusal only when the owner (or, with no usable owner, every candidate) is shown not running; launchd refuses it with a message to boot the job out first. For a container owner `docker inspect` must report it not running or absent; for a launchd owner the job must be unloaded. A Docker error reading an owner's state means unknown, never "not running", so the guard refuses. An unparsable `state.json` refuses before the guard runs.

**Cutover (L3).** A command such as `gt daemon cutover --to container` runs five steps, each with a stated budget and a stated ending on timeout, all waits on an injected clock: (1) wait for a quiet gate, 30 minutes; (2) check quiet again immediately before the next step; (3) boot out the launchd job and wait for the old daemon to stop (the shutdown budget plus 30 seconds), and if it keeps writing, do not start the container and print the command that restores the launchd job; (4) start the container and wait 2 minutes for `state.json` to show the container as owner, and on timeout print the rollback command and the `docker logs` command without rolling back by itself; (5) wait six minutes for a second heartbeat from the container and report unhealthy with the rollback command if none comes. `--to launchd` is the rollback and has the same shape. The quiet gate is the same hazard the install has: the dispatch hold is on (the operator creates it, the command only reads it), no `gt sling` is running, and the container-gate slot is not busy, and an error from any of the three counts as not quiet. Each step goes through the guard; the old owner's clean stop is what lets the new one start.

## 6. The host agent and the probe file

A Go subcommand under its own launchd job (a template, no shell) writes `.runtime/host/probes.json` atomically every 60 seconds: `v`, `at`, free memory percentage, the town volume's disk use, the running container names, CPU idle, top consumers and exec-tax milliseconds. Each field carries its own error string, so one failed read does not zero the rest. The agent runs the Docker CLI with an explicit `--context desktop-linux` and never touches the global context.

In container mode the daemon uses a `fileHostProbes` beside `systemHostProbes`. A missing file or one older than three intervals is an error, so the attention rules report a failing probe and never read it as healthy. `parseMemoryPressure` moves into a small shared package so the agent and the tests share it. The daemon's burst sampler keeps owning `bursts.jsonl` and reads the same file in container mode: one writer.

## 7. The tier sweep, Dolt and the image

**Sweep.** Delete `runner_idle_command`, `runner_idle_wait`, the idle poll and its constants. The config loader tolerates the removed keys with a one-time warning, so an old `settings/config.json` does not stop the daemon, and the keys come out of the live config afterwards. `sweep-dind` uses the same pinned image digest as Forgejo's engine, is privileged, has its own volume, and is reachable only on the project's internal network. The first sweep is slow while images pull.

**Dolt endpoint.** The Mac needs `127.0.0.1:3307` and the container needs `host.docker.internal:3307`, and `town.json` is one value for both. A new `dolt.container_host` field, used only in container mode, gives the container its own host, and the daemon exports it to the `bd` processes it starts. The scratch town has its own `town.json` and Dolt service and never needs it. Docker Desktop normally lets a container reach a service the Mac bound to loopback through `host.docker.internal`; that is not verified for Dolt, and the first plan task probes it.

**Dolt CLI.** `jsonl_git_backup.go` and parts of `doltserver` run `dolt` commands. The container will not hold the Dolt data directory, so an audit finds every call that needs local files and makes it work over the server connection or refuses in container mode.

## 8. The ladder and its proof gates

These are owed by the overseer or the operator and are not acceptance boxes, because a polecat cannot tick them.

- L1: the container is up, the patrol watchdog is green for 30 minutes, the probe file is fresh, and a hand-started second daemon is refused.
- L2: one bead is dispatched and landed through the scratch Forgejo.
- L3: the cutover, 24 hours healthy, and a rehearsed rollback to launchd.

## 9. Failure rules and tests

A stale or missing probe file is an error. A Docker error while reading supervisor state is "unknown", never "not running". A missing compose file is an error. Tests are pure functions with fakes, no sleeps and no Docker in the gate, per `docs/testing.md`. Every refusal row has a test that goes red when the guard is removed. Compose rendering is validated with `docker compose config` in the integration tier only, with a `triggers.txt` row.

## 10. Risks and open items

- A second daemon on the same town root is the worst failure. The heartbeat guard, the launchd boot-out in the cutover command and the L1 check cover it, and the first audit task checks the call sites that start a daemon.
- `sweep-dind` is privileged and accepts unauthenticated connections: anyone who can reach its port can drive that engine and, being privileged, its host VM. Its only protection is the network: declared `internal: true`, no published ports, and only the daemon service joins it, each checked by a test. TLS would add certificate management for a single-host engine; revisit it if the engine ever leaves its compose project.
- `om` cannot use the Mac keychain login in a container. Its credential is decided before L3 under `claude-q3f.7` and `claude-2or.3`.
- Unverified: `host.docker.internal` reaching a loopback-bound Dolt; whether `internal/session` calls Docker on the daemon path (two references, likely the retired Docker-driver path); which `dolt` CLI calls need local data.
- How `gt` commands on the Mac that nudge or peek a seat reach a container daemon is not a concern for sandbox seats (tmux runs inside the sandbox and the host client execs through `openshell`), and the audit confirms it.

## 11. Out of scope

A control cluster, any cloud step, moving Dolt or Forgejo, replacing launchd for the other jobs (the OpenShell gateway, the watchers, Forgejo's maintenance jobs), the dashboard, and the `om` credential decision itself.
