> Status: historical (2026-10). Merged in: gt-jzwoz (design and plan; the implementation beads are gt-ic4sf, gt-dwlbj, gt-xk9my, gt-0rtgc and the rest listed in the plan). Not maintained.

# Tier sweeps at half the time, in their own container (design)

Author: dunlin. Date: 2026-10-30. Status: proposed (operator direction 2026-10-30; overseer decisions the same day). Evidence: `~/gt/.runtime/overseer/birds/tier-sweep-findings.md` (read it first; every number below is from it). Plan: `2026-10-30-tier-sweep-speed-plan.md`.

## 1. Problem

The hourly tier sweeps took 41 minutes on gastown (10:27), 27 on devops (10:54), and earlier 14, 15 and 31. The operator calls it technical debt and wants it cut by at least half, run in its own container, and kept off the daemon.

The measurements say three different things are wrong, and only one of them is slow tests:

1. **Serial execution on a contended host.** Gastown's 66 integration packages run one after another (`internal/tiersweep/gotier.go`), at `nice 10`, under the container-gate slot. Quiet, that is 10-12.5 minutes of integration plus 3-4.5 of shell plus 0.5 of race; loaded, the same tests run 2.5x slower (test time 1473 s against 524-584 s) and the sweep reaches 41 minutes. One package, `./internal/cmd`, is a third of the quiet tier (196 s median). Compile and link is a steady 15%.
2. **A queue wait that is not test time.** Devops took 26m54s for 4m26s of tests: 22m27s was the slot wait (an "unwrapped container suite" verdict plus an install-pending yield). One sweep in 74.
3. **Coupling to the daemon.** A sweep delays installs and dispatch (28 upgrade-restart waits in two days; a restart cut a running sweep at 17m58 and it reran from zero), and the daemon's tick and sling times are somewhat slower during sweeps (heartbeat gap +9%, sling median +28%; correlational).

Wizards: the operator's suspicion is right for the devops landing gate (the wizard harness is the largest part of the 110 s devops unit tier) and for host-rehearsal (87% of the devops sweep's test time runs wizard-successor scripts as processes), but the wizards proper are not in the sweep, and the 27-minute sweep was a queue.

## 2. Targets and how they are measured

Same sweep before and after: `tier-sweep integration race` (and `shell`) at a fixed sha, three runs, median, once when `uptime` shows load1 under 10 (quiet) and once with the town running normally (loaded). The per-package `timings.jsonl` the first bead adds is the instrument; the daemon's `swept ... in` line and the new slot-wait field give the wall.

| Rig / tier | Now (quiet / loaded) | Target | Mechanism |
|---|---|---|---|
| gastown integration + race | 11-13 min / 19-31 min | 5-6 min quiet (2x or better); loaded p90 under 15 min | workers, longest first (S7); `internal/cmd` in 3 shards (S9, S10); nice 0 if the A/B says so (S5); gt built once (S8); then the runner takes load out of it |
| gastown shell | 3-4.5 min / 10.6 min | under 2.5 min quiet | the two scheduled deletions (kind_test 214 s loaded, deep-review_test 127 s) take loaded 10m36 to about 4m54; the shell-tier-to-Go program does the rest |
| devops sweep | 3m19 typical, 27 min worst | worst case under 5 min; typical under 2 min | own engine removes the slot wait; ordering host-rehearsal's largest tests first; wizard-successor components ported in wall order |
| devops unit tier (every landing gate) | 110 s wall | under 60 s | extract the run-rule table check (DV1) and retire wizards in wall order (section 6) |
| daemon during a sweep | heartbeat +9%, sling +28% | within 3% and 10% | the runner (no sweep process on the host) |

The loaded numbers are the point: a sweep whose wall does not move with host load is the goal of the runner, and the targets above for loaded runs only hold with it.

## 3. Approaches considered

A. **Tune in place** (workers, shards, nice, build gt once, resume, cleanup). Cheap, and it takes quiet gastown to 5-6 minutes. It cannot fix contention or the daemon coupling, because the sweep still runs on the host's cores beside the daemon and the seats.
B. **A separate runner** that runs the sweep in a container with a reserved CPU share and its own container engine, the daemon only scheduling and reading `result.json`. Fixes contention and coupling; costs a runner, an image change and a host-only list.
C. **Skip unchanged packages** (a per-package trigger like `tiertrigger`, for all 66). Saves the small packages, none of `internal/cmd` (its dependency closure is nearly the whole repo), and needs the cache-honesty rule (6) satisfied. Left as a later bead, not planned now.

Recommendation, as decided: A first (its beads are independent of the container and pay in quiet minutes), then B; C only if the timings show a long tail worth it. The ordering in the plan is measure, then the container move, then package-level speed-ups, with the cheap host-side items first because they do not wait for the container.

## 4. The runner

**Shape.** `internal/sweeprunner` renders a `Spec` (rig, sha, image, cpus, memory, tiers, jobs, output and repo directories, dind or not) through a `Backend` interface: a Docker backend (argv steps, against a configured Docker context) and a Kubernetes backend (a `batch/v1` Job), both from one validated Spec. `cmd/sweep-run` is the entrypoint inside the container: clone the rig's bare repo at the sha, run `tiersweep.Run` in process, write `result.json` and the logs to `/out`. The daemon (held bead R7) renders, starts it detached, polls for `result.json`, converts it to the state file's existing tier results, and files the same attention items. The runner survives a daemon restart, so a restart no longer cuts a sweep.

**Image and caches.** The polecat image already has Go 1.27.1, libicu, tmux, PostgreSQL 17 and the seeded `GOCACHE`/`GOMODCACHE`; the probe ran ten of twelve integration packages in it first time (a cold start added 2-5 s per package; `cmd/gt` took 54 s including the gt link). It lacks `dolt` and the entrypoint binary (held bead R5).

**Dolt and Docker.** `internal/testutil` starts Dolt through testcontainers, so it needs a Docker API: a Docker-in-Docker sidecar (privileged, on an internal network with no published ports, the only privileged container) is the first step and needs no harness change. An external-Dolt mode in the harness is the later step that removes the sidecar. The shell tier and macOS-bound packages stay on the host, listed in `internal/testpolicy/sweep-host.txt` (R6, seed = the packages whose tests mention darwin, launchctl, osascript, codesign or /Library; the first full runner run confirms each).

**CPU.** "Dedicated" has to be real. The Docker Desktop VM is 8 CPU/16 GB and its CI runner's engine alone was at 322% CPU; the Colima gt-sbx VM that hosts the seats is 16 CPU/50 GB; the host has 24 CPU/128 GB; the kind cluster is down. A pod in a busy VM is not isolation. The runner gets requests equal to limits, and either a node/VM of its own (8-12 cores) or, in the existing VM, the `gt-ci` priority (shed first under pressure, which is the right policy for a sweep). The operator decides which VM (section 7).

**Results and isolation.** The runner has no Docker socket mount and no writable repo mount; it reads the bare repo read-only. It writes only `/out`. `result.json` is versioned and strictly read; a missing or torn file after the stage budget is RED with reason `runner: no result`, never GREEN.

## 5. Reconciliation with the daemon-container plan

`docs/plans/2026-10-28-daemon-container-plan.md` runs the sweep inside the daemon's compose project with a `sweep-dind` engine (C1, C2), and the daemon image carries no Go toolchain. What changes (to be told to its owner, seagull):
- C1 (delete the runner-idle wait) stands.
- C2 (`sweep-dind`) is reframed: the engine is the runner's service or the engine the daemon launches runner containers into, not where tests run inside the daemon container. If the daemon is containerized, its launch seam needs a Docker client or the Docker HTTP API; the plan's rule that the daemon container has no `docker.sock` mount stands, so the engine is reached over its internal TCP address.
- The daemon image does not need Go for the sweep; the runner image has it.
- The sweep is no longer a reason for the daemon container to carry build tools.

## 6. Devops: wizards in wall-time order

The devops unit tier (every landing gate) runs `ops/wizards/...`: 110 s wall at load 38. Summed subtest seconds by package, which is the share of the work: `harness/auth-broker-run-rule` 1992 (46%), `harness/auth-broker-credentials` 1019 (23%), `harness/install-deploy-runner` 558 (13%), `ops/wizards` itself 364 (8%; the add-app family is about 70 s), `harness/wizard-lib` 296 (7%), `harness/platform-cleanup-compute` 230 (5%), `harness/create-keys` 72, `harness/update-app-workflows` 57. Retire in that order: auth-broker-run-rule first. Its harness runs the 1281-line wizard once per rule: 16 `tbl-*` and 13 `unattended-*` cases, 745 s of its 1992 (37%), prove a 90-line embedded python table check. DV1 extracts that check to Go with a table test; the follow-up (shaped when DV1 lands) makes the wizard call it and collapses 29 cases to two. `platform-cleanup-compute` is a tranche-1 deletion with no port. `create-keys` stays by ruling. `wizard-lib` goes with the last wizard. For each wizard that stays until its port, the table families (argument and table validation) are the ones to extract; whole-wizard rehearsals stay as a few cases.

Host-rehearsal (87% of the devops sweep's test time, 228 s wall at 6-way parallel): `gcp-init-stacks` 26%, `gcp-bootstrap` 24%, `gt-root` 9%, `gke-reap` 8%, `publish-rule` 7%, the bash helpers 7%, `patrol-six` 5%. These are the wizard-successor `apply.sh`/`check.sh` components run as processes; they shrink as dv-fec7's tranche 2 and 5 port them, in this order.

## 7. Decisions for the operator

1. Where the runner's CPU comes from: a VM/node of its own (recommended: 8-12 cores, 24 GB), or the existing Colima gt-sbx VM at `gt-ci` priority.
2. Who starts the runner while the daemon still runs under launchd: the daemon, through the Docker backend against a configured context (recommended; the same seam later targets Kubernetes).
3. Whether the tier-sweep cleanup list (the 9120 directories) is run after a printed, reviewed list (recommended; C1 prints, `--apply` deletes).
4. The devops sweep: keep the 127-line `scripts/tier-sweep.sh` for now, or put devops on the Go sweep (a larger change; recommended after the gastown runner works, because devops adds a `-json` host-rehearsal run and its own race and shell lists).

## 8. Risks

- Parallel packages and shards share fixed ports and paths (`internal/doctor`, `doltserver`, `tracker/server`, `/tmp/...` names in `internal/cmd`); the seed `serial.txt` is conservative and shrinks only from the overseer's concurrency runs. Sharding `internal/cmd` is off until a quiet live run is recorded.
- The Linux run of all 66 packages is unmeasured (12 probed): the first full runner run lists failures, and R6's manifest takes the host-bound ones.
- The nice-10 hypothesis is not proven; S5 only adds the knob for an A/B.
- Any speed-up must not weaken a test: no skip, no retry, no raised timeout; shard coverage is proved (listed equals run).
