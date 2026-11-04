> Status: historical (2026-11). Merged in: gt-gg9hv.1 (the operator approved this design on 2026-11-04; the work is the beads under gt-gg9hv). Not maintained.

# The town in Kubernetes: design

## 1. Goal

Every piece the town needs runs in the control cluster, not on the Mac. The Mac becomes a terminal. Three services stay central, outside the cluster, reached by address: Dolt, Postgres and the hm embedder (Ollama). There are no deadlines: the operator decides when each step happens.

This builds on decisions already made on the maps claude-gkl (town in the cloud, confirmed 2026-11-01), claude-2or (control, work and tenant clusters; resident order Forgejo, then the daemon with the landing worker and om) and claude-29a (one town per workspace, Postgres central behind a DSN). It does not reopen them.

## 2. Decisions (operator, 2026-11-04)

| # | Decision | Why |
|---|---|---|
| D1 | All town pieces go in the control cluster; the Mac keeps nothing the town needs to run | the Mac is a terminal (claude-q3f.12, 2026-10-24) |
| D2 | Dolt and Postgres stay central, outside the cluster, reached by DSN | claude-29a Tenancy; they map to a managed database in the cloud later |
| D3 | The hm embedder (Ollama) stays on the Mac for now, central like the databases; the reranker is Jev, so there is no reranker to place | the embedding model is the only part that needs the Mac's GPU |
| D4 | Docker Desktop stays as the home of the central databases; it only needs to shrink | nothing is gained by moving the databases into another VM |
| D5 | kind everywhere; `gt-control` is rebuilt as a kind cluster with core and privileged pools | one tool; node pools match GKE; the work cluster is already kind |
| D6 | The devops repo owns deployment; the gastown repo supplies the images and specs | devops is the designated deploy system |
| D7 | Images are built by CI on every green main commit and deployed by digest; the in-cluster runner applies rendered manifests | no state backend to host; reuses the `IMAGE <name> <tag> <digest>` contract of devops' `images.yml` |
| D8 | The cloud deploy VM stays as it is for now | it holds cloud credentials, so its placement is a separate decision |

Open, not decided: where the cloud deploy VM goes (its own cluster, or a tainted pool in the control cluster); whether and how the overseer and bird agents move; the final home of hm's embedding inference.

## 3. Target architecture

| Piece | Runs in | State |
|---|---|---|
| gastown daemon (patrols, landing worker, plugin runner) | control cluster, one pod | the town root, on a durable volume |
| om | inside the daemon pod | none |
| Forgejo, CI runner | control cluster | Forgejo's data volume; its database stays a central Postgres |
| tier sweeps | Jobs in the control cluster | none |
| dashboard | control cluster | none |
| OpenShell gateway and sandboxes | work cluster (`gt-sbx`, kind) | none |
| image registry | the Forgejo registry, pulled by digest | Forgejo's volume |
| hm (CLI, sync CronJob) | control cluster | central Postgres; the embedder is a central URL |
| town-stats, usage-attrib, spend watch | CronJobs | files on the town volume |
| Dolt, Postgres, the Ollama embedder | central, outside the cluster | their own volumes and backups |
| cloud deploy runner | its current VM | unchanged |

Retired with the move: `gt update`'s host install, `rebuild_gt`, `rebuild_om`, `rebuild_hm`, the host agent, the `polecat-image` and `tool-updater` plugins, the launchd daemon job.

## 4. Deployment

Today there are seven deploy mechanisms (compose, hand-applied YAML, scripts, Helm, launchd, Go renderers, the Mac build path). The design reduces them to one:

1. A Forgejo workflow in the gastown repo builds the `daemon` and `polecat` images on every green main commit, pushes them to the Forgejo registry, and prints one `IMAGE <name> <tag> <digest>` line each. `bd` and `om` are built from commits pinned in one file.
2. The devops repo renders the manifests from a typed spec and the digests. The renderer is `pkg/jobhost` extended for what the town needs: a singleton stateful service (volume claims, mounted Secrets, a service account with a Role, a recreate rollout) and CronJobs, plus a target for any existing cluster. No state backend: the cluster is the state, and apply is idempotent.
3. The in-cluster runner applies the rendered manifests with a service account. `gt update` keeps its drain, maintenance, install, restart, verify and lift steps; its install step becomes "set the new digest and wait for the rollout".
4. The polecat image's digest goes into the sandbox profile and sandboxes pull it from the same registry.

A person introduces the workflow file: devops' pre-receive hook lets only the human change workflow files.

## 5. Options considered

| Option | Verdict |
|---|---|
| `internal/deploytree` | not used: built for the hosted multi-workspace future around `trackerd`, nothing calls it, its output was not run under kind |
| `pkg/jobhost` as it is | extended, not used as is: no volumes, secret mounts or RBAC; its kind path keeps Pulumi state on the Mac |
| Pulumi for the town | not used: needs a state backend; it stays for GKE and Cloud Run |
| Argo CD or Flux | later: self-healing is worth adding once there are several workspaces |
| k3s for the control cluster | replaced by kind: single node cannot isolate the runner pool, and the work cluster is already kind |
| a `gt-data` VM, or retiring Docker Desktop | dropped: solves a problem the operator does not have |
| a host executor that runs Mac-only plugins for the container | dropped: it keeps work host-resident behind a queue |

kind clusters lose their volumes on a rebuild. The durable volumes (the town root, Forgejo's data) are therefore mounted from paths in the VM, not stored inside the node containers, and Forgejo's existing backup stays the safety net.

## 6. What running the daemon in a container taught us

Four root causes explain the failures seen after the 2026-11-04 container cutover; the plan fixes each at its root.

1. **The town directory is shared but its tools are the Mac's.** Plugin binaries under `plugins/*/bin/` are macOS builds (exec format error); `rebuild_*` and `update` read host-installed binaries; the entrypoint's config paths differ (`XDG_CONFIG_HOME=/run/gt/xdg`), so the landing worker could not find its bot token and landed nothing.
2. **"Dolt host is not loopback" is read as "the data directory is not here."** The maintenance guard skips; the JSONL backup export runs `dolt sql` without `--host` and opens the data files directly. That worked on the Mac only because file locks held by the Dolt container do not reach the host (reads can be stale); in a container the lock is visible and it fails.
3. **Tier sweeps depend on the Mac's Docker contexts** (`docker --context colima-gt-control` to save and load the image).
4. **The compose generator cannot express** the Forgejo host alias or the config directory mount.

Two bugs the cutover also exposed are fixed and in review: the startup guard asked `launchctl` whether the old owner ran (gt-4r9wf), and `EnsureMetadata` wrote the container's Dolt address into the beads `metadata.json` files the Mac reads (gt-fimtw).

## 7. hm

Measured 2026-11-04 on the 4.3 GB qwen3-embedding 4B q8 model: one query takes 0.03 s on the Mac GPU and 0.24 s on 8 CPUs in a container; chunk embedding runs at 7.2 chunks/s on the GPU and about 1 chunk/s on CPU, so a full reindex of 135,741 chunks is about 5 hours on the GPU and about 39 hours on CPU. The index gains 1,100 to 14,600 chunks a day. The system uses hm as an optional helper (om's review context, `gt spec lint` hints, the Jev dedupe command, the daemon's sync patrol), and all of them degrade without it.

Staging: run the hm CLI, the sync CronJob and the central Postgres client in the cluster against the Mac's embedder now; evaluate the 0.6B model on hm's 120-query golden set; if its quality holds, run embedding in a CPU pod and the Mac exception ends.

## 8. Prior art

claude-gkl, claude-2or (.1, .2, .9), claude-29a, claude-qpy, claude-xcm, claude-q3f.12, docs/research/openshell-gastown/control-plane-macos-dependencies.md (30 rows, 2026-10-22), devops docs/design/jobhost.md, gastown docs/design/k8s-sandboxes.md, the daemon-container program gt-qpeei and docs/daemon-container.md.
