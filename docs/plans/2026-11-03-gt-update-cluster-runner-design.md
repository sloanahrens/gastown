> Status: historical (2026-11). Merged in: gt-sfveb. Not maintained.

# gt update: build and image publish in the control cluster

## Goal

The first draft of this design moved the two heavy steps of `gt update`, building `gt` and building and publishing the sandbox image, off the host into the control cluster (`colima-gt-control`), the way the tier sweeps already run. The reason was cost: the image build took 8 to 20 minutes of host CPU and disk beside the polecats (gt-jmd9n). This revision measures that cost again after the seed was removed from the image (gt-nposw, gt-yvnsa) and decides what is still worth building.

## What was measured (2026-11-03)

| Measurement | Result |
|---|---|
| Cold build of the old image (seed RUN) in a dind sidecar at 4 GiB and 2 CPU | failed with ResourceExhausted after 19m22s; 14 minutes of that in the devops rig's `go build ./...` (Pulumi SDKs); peak 4.09 GiB |
| Cold build of the new image (no seed RUN) in the same sidecar | 119 s, exit 0, peak 2.9 GiB; image 2.05 GB (was 8.86 GB) |
| Push of that image to the registry through the Colima forward, `docker push` with `--insecure-registry` | 31 s |
| `crane copy --insecure` of the golang image from a pod to `host.lima.internal:5001` | 100 s including the pull from Docker Hub; `crane digest` read it back |
| Registry reachable from a control-cluster pod | HTTP 200 on `host.lima.internal:5001/v2/` and on `host.docker.internal:5001/v2/` |
| `gt` cross-compile darwin/arm64 in a `golang:1.26` pod, `CGO_ENABLED=0` | 33 s cold, 1.9 CPU and 1.7 GiB at the sample, 56 MB binary |
| The real `gt update` on the host (run 20261104T013211Z-05bf9a4d) | 3m11s in all: maintenance 15 s, install 1m41s (builds `gt`, builds the image in 18 s at that commit, pushes it in about a minute), daemon restart wait 1m15s |
| Control node while a sweep runs | the sweep requests 6.2 of 8 CPUs; a 2-CPU pod stayed Pending until the sweep ended |

## What changed

1. **The cost the cluster builder was meant to remove is gone.** The host image build is 18 s, not 8 to 20 minutes, because the image no longer compiles every rig. A cluster builder would save about 18 s of host CPU and a minute of push per update.
2. **The two `gt` binaries are not the same artifact.** `make build` builds `gt` with the host toolchain and cgo on (the installed binary reports `CGO_ENABLED=1` in `go version -m`), and the cluster cross-compile above had to turn cgo off. Shipping the cluster's binary would install a different build of `gt` than the one every test and sweep covers. That alone rules out building `gt` in the cluster.
3. **A builder Job and a sweep cannot share the control node** at its present size (8 CPUs, sweeps ask for 6.2). The builder would queue behind sweeps, which run for 15 minutes an hour.

## Decision

Do not build the cluster-runner backend now. Keep `Builder` as a recorded option with these triggers, and revisit when one fires:
- the host image build exceeds 5 minutes again (for example a seed returns),
- the update must run on a machine with no Docker, such as a move of the town off this Mac,
- the control cluster grows enough to hold a sweep and a builder together.

## What is still worth building: consumer-side verification of the published image

The first draft verified the image by comparing the registry digest with "the digest the Job logged". The om review (finding 18e7fccc0c3d) was right that this checks the producer against its own report. The same weakness exists today on the host path: `BuildAt` checks the label on the local image, then pushes, and nothing reads the registry afterwards. After the push, `gt update` should read back from the registry what a sandbox will pull:

1. `crane digest --insecure <registry>/gastown-polecat:<short-commit>` and `crane digest` of the `:dev` tag must be equal. Both Kubernetes sandbox profiles pull `localhost:5001/gastown-polecat:dev` (`settings/config.json`, `sandboxes.polecat-k8s.image` and `sandboxes.sbx-haiku.image`), so this proves the tag they pull is the one just published.
2. `crane config --insecure` on that digest must carry the label `org.gastown.gt-commit` equal to the full target commit, and `org.gastown.bd-commit` equal to the value on the local image the build just made, which the install step reads with the same `docker inspect` label call `BuildAt` uses for the gt commit (`internal/polecatimage/build.go`) and records in the update state before the push. This is the check `BuildAt` does on the local image, made on the object the registry serves.
3. A mismatch fails the install step before the daemon restart, and the update state records the digest and the two labels it read.

This needs no cluster, uses `crane` as the publisher already does (`internal/polecatimage/publish.go`), and covers the host path today and any later cluster builder. It is one bead.

## Security posture, stated

The sidecar of a sweep Job is privileged; a builder would be the same, in the same single-user local VM with no inbound exposure (the posture the sweep runner already has, gt-7zy61). The registry is plain HTTP and unauthenticated, bound to the gt-sbx VM's loopback and forwarded by Colima to the Mac's loopback, so any process that can reach the Mac's loopback can read or write it.

What the read-back check does and does not do. It catches a publish that went wrong: a push that failed half way, a tag that points at an older build, an image built from a different commit than the install. It does not defend against a writer to the registry: the two labels are self-asserted metadata a writer can set to match, and `:dev` is a mutable tag, so a write between the read-back and a sandbox's pull is not caught. A process on this Mac that can write the loopback registry can already run code as the same user, so the registry is not a separate trust boundary here and this design does not add one. If the registry ever leaves the loopback (the GKE move), pin the profiles to the digest the update job records (`image@sha256:...`) and sign it; that is a separate design and is not claimed here.

## Out of scope

Moving the install, the restart or the hold into a cluster; a second registry; Dolt or Postgres; any change to how `gt` itself is built.

## Beads

| # | Title | Depends on | Risk path |
|---|---|---|---|
| 1 | `internal/polecatimage`: read the published image back from the registry (digest of both tags equal, label commits equal to the target), with an injected `crane` runner and unit tests | none | no |
| 2 | `gt update` install step: call it after the push, fail before the restart on a mismatch, record digest and labels in the state | 1 | yes (daemon-adjacent install path) |

The cluster-runner beads from the first draft (Job renderer, `ClusterBuilder`, the `builder` setting) are not filed; the first draft's table is in this file's history (commit 9e120aba) if a trigger fires.
