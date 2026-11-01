# Control cluster on the gt-kind VM (gt-9v1np)

The VM is `gt-kind`. The Colima VM named `gt-control` is NOT the control plane: it is the sweep runner's dedicated engine and stays isolated from everything here. The work cluster (sandboxes, the cache, sweep jobs) stays `gt-sbx`.

## What this directory holds

`kind-config.yaml` is the cluster `gt-ctl`, every node on the pinned node image the gt-sbx cluster already runs: one control-plane node, a `core` worker (`gt.dev/pool=core`) and a `privileged` worker (`gt.dev/pool=privileged`, tainted `NoSchedule`) for the Forgejo runner later. Nothing here starts a VM or a cluster.

## Host core budget

The host has 24 cores. The Colima VMs are allocated as follows (allocation, not use): Docker Desktop 8, gt-sbx 12, gt-control 8, gt-kind 6, which is 34 against 24. Starting gt-kind adds 6. The total comes down only when the Forgejo compose stack and its docker-in-docker runner leave Docker Desktop for this cluster (later beads); only then is Docker Desktop's allocation lowered. `hm-postgres` stays a Docker container under the standing rule (a central source, not a pod).

Start size is 6 CPU / 12 GB, enough for hm. Grow to 8 CPU / 16 GB before Forgejo and its runner move in (stop the VM, start it again with the new size: a running VM keeps its size).

## Start (the overseer, after the L3 window's first checks are green)

The commands set the docker endpoint per command and never switch the global docker or kubectl context.

```
colima start gt-kind --activate=false --cpu 6 --memory 12 --disk 40 --vm-type vz --runtime docker
mkdir -p ~/gt/.runtime/ctl-k8s
DOCKER_HOST=unix://$HOME/.colima/gt-kind/docker.sock kind create cluster --config deploy/k8s/control/kind-config.yaml --kubeconfig ~/gt/.runtime/ctl-k8s/kubeconfig --wait 180s
kubectl --kubeconfig ~/gt/.runtime/ctl-k8s/kubeconfig get nodes -L gt.dev/pool
```

Expected: three nodes Ready, pools `core` and `privileged` on the two workers. Smoke test: apply any one-pod Deployment with `nodeSelector: {gt.dev/pool: core}`, see it Running, delete it.

## Stop and rollback

```
DOCKER_HOST=unix://$HOME/.colima/gt-kind/docker.sock kind delete cluster --name gt-ctl
colima stop gt-kind
```

Nothing else depends on this cluster until a workload is moved into it, so stopping it changes nothing live.

## Environments

Hosts, the registry and the storage class are meant to come from per-environment values files, not to be spelled in a manifest (gt-ht5he adds them under `deploy/k8s/values`; until it lands there are none).
