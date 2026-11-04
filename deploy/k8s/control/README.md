# Control cluster on the gt-control VM (gt-9v1np)

The cluster `gt-ctl` runs kind on the Docker engine of the existing Colima VM `gt-control`; there is no separate control VM. The work cluster (sandboxes, the cache, sweep jobs) stays `gt-sbx`.

## What this directory holds

`kind-config.yaml` is the cluster `gt-ctl`, every node on one pinned node image, the one the gt-sbx nodes ran on 2026-11-01: one control-plane node, a `core` worker (`gt.dev/pool=core`) and a `privileged` worker (`gt.dev/pool=privileged`, tainted `NoSchedule`) for the Forgejo runner later. Nothing here starts a VM or a cluster.

## Durable volumes

A kind cluster loses its node containers, and the volumes they hold, when it is rebuilt. The core worker therefore mounts the VM path `/var/lib/gt-control/volumes` at `/var/local-path-provisioner` in the node, so what the local-path provisioner writes survives `kind delete cluster` and a recreate. Create the directory on the `gt-control` VM before the cluster starts:

```
colima ssh -p gt-control -- sudo mkdir -p /var/lib/gt-control/volumes
```

## Host core budget

The host has 24 cores. The Colima VMs are allocated as follows (allocation, not use): Docker Desktop 8, gt-sbx 8, gt-control 12, which is 28 against 24. The local total comes down only when the Forgejo compose stack and its docker-in-docker runner leave Docker Desktop for this cluster (later beads); only then is Docker Desktop's allocation lowered. The cloud VM is separate and is not part of this count. `hm-postgres` stays a Docker container under the standing rule (a central source, not a pod).

## Start (the overseer, after the L3 window's first checks are green)

The commands set the docker endpoint per command and never switch the global docker or kubectl context. The `gt-control` VM is already running for the sweep runner; this cluster shares its engine.

```
mkdir -p ~/gt/.runtime/ctl-k8s
DOCKER_HOST=unix://$HOME/.colima/gt-control/docker.sock kind create cluster --config deploy/k8s/control/kind-config.yaml --kubeconfig ~/gt/.runtime/ctl-k8s/kubeconfig --wait 180s
kubectl --kubeconfig ~/gt/.runtime/ctl-k8s/kubeconfig get nodes -L gt.dev/pool
```

Expected: three nodes Ready, pools `core` and `privileged` on the two workers. Smoke test: apply any one-pod Deployment with `nodeSelector: {gt.dev/pool: core}`, see it Running, delete it.

## Stop and rollback

```
DOCKER_HOST=unix://$HOME/.colima/gt-control/docker.sock kind delete cluster --name gt-ctl
```

The VM keeps running for the sweep runner. Nothing else depends on this cluster until a workload is moved into it, so deleting it changes nothing live; the durable volume path stays on the VM and is picked up again when the cluster is recreated.

## Environments

Do not spell a host, the registry or a storage class in a manifest here: take them from the per-environment values files under `deploy/k8s/values`.
