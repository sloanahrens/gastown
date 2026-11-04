> Status: current (2026-11). The plan for 2026-11-04-town-in-kubernetes-design.md; epic gt-gg9hv. Each row is one bead of at most twenty minutes of one worker; rows marked "overseer" are done by the overseer in a crew worktree.

# The town in Kubernetes: plan

Phases are ordered by dependency, not by date. Nothing here has a deadline; the operator starts each phase.

## Phase 0: make the container daemon whole (works today, carries into the cluster)

| Id | Repo | Task | Needs | Acceptance sketch |
|---|---|---|---|---|
| P0.1 | gastown | `gt daemon compose write --extra-host name:ip` (repeatable) renders `extra_hosts` | | unit test: the render carries the entries; a malformed entry is refused |
| P0.2 | gastown | `gt daemon compose write` mounts the town's config directory read-only at its host path and at the `XDG_CONFIG_HOME` path | | unit test: both mounts appear; the file's other mounts are unchanged |
| P0.3 | gastown | the JSONL backup export reaches Dolt through the town's endpoint (`dolt.container_host` in container mode), never the data files | | unit test on the command built: it carries `--host`/`--port` whenever the town names an endpoint |
| P0.4 | gastown | the maintenance guard treats a bind-mounted data directory as local in container mode | | unit test: container mode with the data directory present is not external; otherwise unchanged |
| P0.5 | gastown | the plugin runner resolves `bin/<goos>-<goarch>/<name>` before `bin/<name>` | | unit test: the per-OS binary wins; the old layout still resolves |
| P0.6 | gastown | the daemon image build compiles the four plugins for linux and the entrypoint installs them into the town's plugin directories | P0.5 | unit test on the build plan and the entrypoint step |
| P0.7 | overseer | tier sweeps from the container: the sweep image reaches `gt-control` through a registry, and the container gets its kubeconfig | | a sweep launched from the container reaches GREEN |
| landed or in review | gastown | gt-4r9wf (guard reads the launchd job from the host agent), gt-fimtw (metadata host), gt-9ui7q (runner renderer fixes) | | |

## Phase 1: the pipeline (devops and gastown)

| Id | Repo | Task | Needs | Acceptance sketch |
|---|---|---|---|---|
| P1.1 | devops | `pkg/jobhost` spec: a singleton stateful service (volume claims, mounted Secrets, service account and Role, recreate rollout) | | golden render test |
| P1.2 | devops | `pkg/jobhost` spec: a CronJob component | | golden render test |
| P1.3 | devops | a target for any existing cluster (in-cluster service account or kubeconfig), not tied to the kind setup | | validate test with fake targets |
| P1.4 | devops | `jobhost render` and `jobhost apply` usable from a CI job, no state backend | P1.1, P1.3 | unit test: apply is a server-side apply of the rendered objects |
| P1.5 | devops | workflow template: build named images on green main, push, print the `IMAGE` lines | | template test |
| P1.6 | gastown | the images workflow for `daemon` and `polecat`, introduced by the operator; `bd` and `om` pinned in one file | P1.5 | the workflow prints two `IMAGE` lines |
| P1.7 | gastown | the daemon's spec and values (town volume, Secrets, Role for sweeps) | P1.1, P1.3 | the render passes the spec validation |
| P1.8 | gastown | `gt update`'s install step has a Kubernetes backend: set the digest, wait for the rollout | P1.4, P1.7 | unit test with a fake cluster client |
| P1.9 | gastown | the sandbox profile takes the polecat image digest from the values; the `polecat-image` plugin is retired | P1.6 | unit test; the plugin is removed |

## Phase 2: the control cluster becomes kind

| Id | Repo | Task | Needs | Acceptance sketch |
|---|---|---|---|---|
| P2.1 | gastown | `deploy/k8s/control` builds the kind cluster on `gt-control` (core and privileged pools, durable volumes mounted from the VM), replacing its k3s | | config test; the runbook names `gt-control` correctly |
| P2.2 | overseer | rebuild `gt-control` as kind; restore Forgejo and the runner from the backup; restore proof | P2.1 | a landing passes through the rebuilt cluster |

## Phase 3: the daemon moves into the cluster

| Id | Repo | Task | Needs | Acceptance sketch |
|---|---|---|---|---|
| P3.1 | gastown | the town-root volume: seed and restore runbook | P2.2 | runbook with a restore proof on a throwaway cluster |
| P3.2 | overseer | cut the daemon over from the Docker Desktop container to the pod, with the old container as rollback | P1.8, P3.1 | the pod owns the town and lands a bead |
| P3.3 | gastown | the dashboard as a Deployment | P1.1 | render test |
| P3.4 | gastown | CronJobs for town-stats, usage-attrib and the spend watch | P1.2 | render tests |
| P3.5 | gastown | retire the host agent and the host-side update and rebuild paths | P3.2 | the code and its tests are removed |
| P3.6 | overseer | decide why the OpenShell gateway also runs as a Mac process, and retire the copy it does not need | | recorded decision |

## Phase 4: hm

| Id | Repo | Task | Needs | Acceptance sketch |
|---|---|---|---|---|
| P4.1 | hm | an image with the hm CLI | | image builds in CI |
| P4.2 | devops | the hm sync CronJob, with the embedder URL and the Postgres DSN as values | P1.2, P4.1 | render test |
| P4.3 | hm | evaluate the 0.6B embedding model on `eval/golden.jsonl` against the 4B | | a recorded recall comparison |

## Not in this plan

The cloud deploy VM's placement, the overseer and bird agents, the final home of hm's embedding inference, GitOps controllers, and the cloud (GKE) step.
