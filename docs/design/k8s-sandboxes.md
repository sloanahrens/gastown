# Kubernetes sandboxes

DeepSeek polecats run in OpenShell sandboxes on Kubernetes, first on a local kind cluster, then in the cloud from the same files. The Docker-driver seat on the Mac stays as the control until five beads have landed through the Kubernetes seat. This page is the shared plan; the tracker is the wayfinder map `claude-wkb` in `~/.claude`.

## Principles

- **Same artifacts, different values.** `deploy/k8s/` holds the install and one values file per environment (`values/kind.yaml` today, a GKE file later).
- **Stable names for the control plane.** Sandboxes reach Dolt and Forgejo as `dolt` and `forgejo` Services. They point at the Mac now; moving either service into the cluster later swaps what stands behind the name, with no policy or image change.
- **The Mac daemon stays the control plane** until the cutover work is decided.
- **Isolated from the town by default.** The kind VM has its own Colima profile, and the gateway is registered in a private CLI config (`XDG_CONFIG_HOME`).

## Decisions (Sloan, 2026-10-24)

1. The gateway is reached on a fixed loopback port through a kind port mapping and a NodePort Service, not `kubectl port-forward`.
2. A local registry runs beside kind; the polecat image is published to it, and the profile image becomes a registry reference.
3. Sandboxes use the stable `dolt` and `forgejo` names.
4. No persistent per-rig Go build cache at first; measure cold builds, then add a module proxy if needed.
5. First seat `deepseek-flash-sandbox-k8s` at cap 1, done when five beads land through it.
6. Order: kind wiring and registry, small backend beads, one bead end to end, then Dolt and Forgejo in-cluster, then GKE.

## Running it

`deploy/k8s/kind/up.sh` builds the environment and prints the `export` line for the next shell; `--register-town` also copies the `k8s` entry and its client certificates into `~/.config/openshell`, where the daemon's `openshell -g k8s` looks. `down.sh` removes it (`--stop-vm` also stops the VM). `kind_test.sh` runs the stub-tool tests without a VM. Before changing a port, the profile or the directory, read the `GT_K8S_*` defaults in `deploy/k8s/kind/common.sh`.

## Things that cost time

- **Chart pin.** Use OpenShell chart 0.1.4. The 0.1.5-pre.4 chart publishes a server certificate that its CA did not sign (`BadSignature`). There is no 0.1.5 chart tag.
- **Manifest pin.** The first fetch records the agent-sandbox manifest's digest beside it, and a changed manifest fails the run; set `GT_K8S_AGENT_SANDBOX_SHA256` to pin it from the first fetch.
- **The first certificate read can be stale.** After install the chart's certificate job can leave the gateway serving a different CA than the one first read from the secret. `up.sh` re-exports the client certificates until `gateway info` is healthy (bounded).
- **Client key format.** The CLI needs a PKCS8 key; escape the dot in jsonpath keys (`tls\.key`) or the file is silently empty.
- **Active gateway.** `openshell gateway add` makes the new gateway active, so a town registration reads the active gateway first and selects it back afterwards; a restore that fails stops the run and names the `openshell gateway select` to run. A rebuild replaces the client certificates, so `--register-town` re-adds only the `k8s` entry on every run. The old entry is removed before the new certificates are written, and an exit trap re-adds it and selects the previous gateway if the run stops in between, so a failed registration never leaves the daemon without `k8s`. A run that finds no active gateway in the town config says so and leaves `k8s` active.
- **Docker context.** `colima start` switches Docker's global context and sends the town's plain `docker` calls to the wrong VM. Every kind and registry call here uses the VM's socket through `DOCKER_HOST`; if colima flips the context anyway, `up.sh` restores the original.
- **Ports.** Someone else may already listen on a port; `up.sh` refuses a busy gateway port and never signals the other process.
- **NetworkPolicy lag.** kindnet enforces ingress and egress policy a few seconds after it is applied; OpenShell creates policies before it starts the workload, so this does not affect it.

## Build beads

`gt-s9gqm` this bootstrap; `gt-mm7q6` publish the image to the registry; `gt-g08vr` a `driver` field on sandbox profiles; `gt-ujzas` create providers on a named gateway; `gt-fju1j` the `dolt` and `forgejo` Services and a kind policy. The proof (five beads on the Kubernetes seat) is a seat-config change the overseer makes.
