> Status: design (2026-11-03, overseer). Track D2 of `2026-11-03-gt-update-plan.md`. Bead gt-sfveb. Not yet planned into beads.

# gt update: build and image publish in the control cluster

## Goal

Move the two heavy steps of `gt update`, building `gt` and building and publishing the sandbox image, off the host and into the control cluster (`colima-gt-control`, 16 GiB), the way the tier sweeps already run. The host keeps the binary swap, the daemon restart, the maintenance request and the dispatch hold, because only the host can do them. The sandbox image build currently takes 8 to 20 minutes of host CPU and disk beside the polecats (gt-jmd9n).

## What the record says

- The image publish already avoids Docker Desktop's insecure-registry setting: it tags the image with the registry reference, writes a `docker save` tarball and pushes it with `crane push --insecure`; extra tags are made with `crane tag --insecure`, which copies no layer (gt-mm7q6, gt-vbv3s; `internal/polecatimage/publish.go`).
- The registry `gt-registry` runs in the gt-sbx VM's Docker, bound to its loopback on 5001, and Colima forwards the port to the Mac's `127.0.0.1:5001`.
- The sweep runner already renders a pod with a privileged docker-in-docker sidecar (2 CPU, 4 GiB) at priority class `gt-ci` (`internal/sweeprunner/k8s.go`).
- `make polecat-image` builds from a detached worktree of the rig repo at the installed commit and checks the label it ends up with (`internal/polecatimage/build.go`).
- Measured 2026-11-03: a pod in the control cluster gets HTTP 200 from `http://host.lima.internal:5001/v2/` and from `host.docker.internal:5001`. A push has not been measured.

## Approaches

1. **Builder Job modelled on the sweep runner (recommended).** A `batch/v1` Job in the control cluster with a runner container and a dind sidecar. It clones the rig repo at the target commit, runs `make gt-installer`-free steps only (`make build` for `gt`, `make polecat-image` for the image), then pushes with `crane --insecure` to `host.lima.internal:5001`. Reuses the sweep runner's renderer, teardown, priority class and memory accounting. Cost: a new renderer for a build spec and an artifact hand-back for the `gt` binary.
2. **Run the existing host build inside a long-lived builder VM.** Less new code, but a second always-on VM and its disk, which the memory work this week just removed.
3. **Keep the host build, add a nice level.** Cheapest, and does not remove the contention; gt-dwlbj showed nice alone does not change sweep time much.

## Design (approach 1)

**Backend interface.** `internal/update` gets a `Builder` interface with `Build(ctx, Target) (Result, error)`. `HostBuilder` wraps today's code; `ClusterBuilder` renders and runs the Job. Selection by `daemon.patrols.update.builder` (`host` default, `cluster`). A pure renderer, like `sweeprunner`, so the Job spec is unit tested with no cluster.

**Two outputs, one Job.**
- The `gt` binary: built for darwin/arm64 in the runner (Go cross-compile, no cgo needed for `gt`), written to the Job's output claim, then copied to the host by the update job (`kubectl cp`) and checksummed against the sha the Job reports. The installer still runs on the host from that file.
- The sandbox image: built in the dind sidecar, pushed to the registry. The host never loads it.

**Published-image check by digest.** After the push the update job asks the registry for the manifest digest of the new tag (`crane digest --insecure`) and compares it with the digest the Job logged. The label check `BuildAt` does today (the image carries the commit) moves into the Job as its last step. The update state records tag, digest and commit.

**Failure behaviour.** Any Job failure leaves the host unchanged and the installed binary and image as they were; the update job marks the step failed and lifts the hold, exactly as for a host build failure. A Job that does not finish within the step budget (30 minutes) is deleted. A registry that does not answer `/v2/` at the start fails the step before the build starts.

**Resources.** The Job asks for 4 CPU and 6 GiB (runner 2 and 2, dind 2 and 4), class `gt-ci`. A tier sweep peaks near 5.4 GiB of 16 GiB; the two fit together, and `gt-ci` is shed first under pressure. The scheduler already serialises sweeps, so the update job waits for a running sweep rather than racing it.

**Dependencies, stated as risks.** The registry lives in the gt-sbx VM: if that VM is down the cluster build cannot publish, and the step fails with a message naming the VM. This is the same dependency the host build has today. The Forgejo and gate-runner moves change where the repo is cloned from; this design clones from the Forgejo remote the sweep runner already uses, so those moves do not block it.

## Out of scope

Moving the install, the restart or the hold into the cluster; a second registry; building the other rigs' images; Dolt or Postgres.

## Measure before building

1. A push of the current sandbox image tarball from a pod in the control cluster to `host.lima.internal:5001`: wall time and whether `crane --insecure` is enough (no daemon setting needed).
2. A cross-compile of `gt` for darwin/arm64 in a runner pod: wall time and peak memory.
3. A dind build of `make polecat-image` in the sidecar with the 4 GiB limit: does it fit, and how long.

If 1 or 3 fails, the design falls back to building the image on the host and moving only the `gt` build, which is the smaller win.

## Beads (sketch)

Each is one polecat-sized bead with a narrow `-run` gate; the daemon-touching ones are a chain.

| # | Title | Depends on | Risk path |
|---|---|---|---|
| 1 | `internal/update` `Builder` interface and `HostBuilder` wrapper (no behaviour change) | none | no |
| 2 | Pure Job renderer for a build spec, golden-file tested | 1 | no |
| 3 | Digest check helper (`crane digest` wrapper with an injected runner) | none | no |
| 4 | `ClusterBuilder`: apply, wait, fetch the binary, delete the Job, with fakes | 1, 2, 3 | yes |
| 5 | Config `daemon.patrols.update.builder` and the step wiring | 4 | yes |
| 6 | Runbook and the first overseer-run rehearsal | 5 | overseer-run |
