# Kubernetes sandboxes

DeepSeek polecats run in OpenShell sandboxes on Kubernetes, first on a local kind cluster, then in the cloud from the same files. The Docker-driver seat on the Mac stays as the control until five beads have landed through the Kubernetes seat. This page is the shared plan; the tracker is the wayfinder map `claude-wkb` in `~/.claude`.

## Principles

- **Same artifacts, different values.** `deploy/k8s/` holds the install and one values file per environment (`values/kind.yaml` today, a GKE file later).
- **Stable names for the control plane.** Sandboxes reach Dolt and Forgejo as `dolt` and `forgejo` Services. They point at the Mac now; moving either service into the cluster later swaps what stands behind the name, with no policy or image change.
- **The Mac daemon stays the control plane** until the cutover work is decided.
- **Isolated from the town by default.** The kind VM has its own Colima profile, and the gateway is registered in a private CLI config (`XDG_CONFIG_HOME`).

## Decisions (the operator, 2026-10-24)

1. The gateway is reached on a fixed loopback port through a kind port mapping and a NodePort Service, not `kubectl port-forward`.
2. A local registry runs beside kind; the polecat image is published to it, and the profile image becomes a registry reference.
3. Sandboxes use the stable `dolt` and `forgejo` names.
4. No persistent per-rig Go build cache at first; measure cold builds, then add a module proxy if needed.
5. First seat `deepseek-flash-sandbox-k8s` at cap 1, done when five beads land through it.
6. Order: kind wiring and registry, small backend beads, one bead end to end, then Dolt and Forgejo in-cluster, then GKE.

## Running it

`deploy/k8s/kind/up.sh` builds the environment and prints the `export` line for the next shell; `--register-town` also copies the `k8s` entry and its client certificates into `~/.config/openshell`, where the daemon's `openshell -g k8s` looks. `down.sh` removes that town entry before it tears anything down, and refuses while `k8s` is the active gateway, since removing it would leave none selected (`--stop-vm` also stops the VM). `kind_test.sh` runs the stub-tool tests without a VM. `up.sh` skips helm when the `gt-bootstrap` ConfigMap's marker matches the chart version and the values digest; `GT_K8S_FORCE_HELM=1` forces a run, since every upgrade regenerates the gateway's certificates. Before changing a port, the profile or the directory, read the `GT_K8S_*` defaults in `deploy/k8s/kind/common.sh`.

## Node pools (town cluster)

The town cluster has a control plane and two workers, `core` and `sandbox`; the labels, the sandbox taint and which workloads each pool carries are in `deploy/k8s/kind/cluster.yaml`, which `up.sh` renders with the cluster name and gateway port, and the eviction order is in `deploy/k8s/priority-classes.yaml`. A node can read only the Secrets of pods scheduled on it, so sandbox pods, which tolerate the taint, share no node with the control plane's secrets.

## Things that cost time

- **Chart pin.** Use OpenShell chart 0.1.4. The 0.1.5-pre.4 chart publishes a server certificate that its CA did not sign (`BadSignature`). There is no 0.1.5 chart tag.
- **Manifest pins.** The first fetch records each downloaded manifest's digest beside it (agent-sandbox, metrics-server), and a changed manifest fails the run; set `GT_K8S_AGENT_SANDBOX_SHA256` or `GT_K8S_METRICS_SERVER_SHA256` to pin one from the first fetch.
- **The first certificate read can be stale.** After install the chart's certificate job can leave the gateway serving a different CA than the one first read from the secret. `up.sh` re-exports the client certificates until `gateway info` is healthy, bounded by `GT_K8S_CERT_TRIES`.
- **Control-plane resources.** `up.sh` gives every container of the gateway statefulset (500m CPU, 1Gi memory) and of the agent-sandbox controller (100m, 128Mi) requests equal to limits, so both pods run Guaranteed and the kubelet evicts them after the BestEffort and Burstable pods under memory pressure. The gateway numbers come from a measured pod memory peak of 1541 MB across its restarts on 2026-11-25; its two containers share that figure, so 1Gi per container covers it where 768Mi per container had no room once the VM was starved (gt-1sf6og). The resource patches restart the pods but do not regenerate the gateway's certificates, so they need no certificate export. A helm values change does regenerate them, so it needs `up.sh`'s certificate export step, which re-exports the client certificates in the same run.
- **Client key format.** The CLI needs a PKCS8 key; escape the dot in jsonpath keys (`tls\.key`) or the file is silently empty.
- **Active gateway.** `openshell gateway add` makes the new gateway active, so a town registration reads the active gateway first and selects it back afterwards; a restore that fails stops the run and names the `openshell gateway select` to run. A rebuild replaces the client certificates, so `--register-town` re-adds only the `k8s` entry on every run. The old entry is removed before the new certificates are written, and an exit trap re-adds it and selects the previous gateway if the run stops in between, so a failed registration never leaves the daemon without `k8s`. A run that finds no active gateway in the town config says so and leaves `k8s` active.
- **Docker context.** `colima start` switches Docker's global context and sends the town's plain `docker` calls to the wrong VM. Every kind and registry call here uses the VM's socket through `DOCKER_HOST`; if colima flips the context anyway, `up.sh` restores the original.
- **Ports.** Someone else may already listen on a port; `up.sh` refuses a busy gateway port and never signals the other process.
- **Image GC.** kind's kubelet garbage-collects no images by default, which once filled the VM's disk (gt-ppgga). Read `deploy/k8s/kind/cluster.yaml` for the thresholds before changing the kubelet's image GC; it applies to every node, so a rebuilt cluster prunes unused images from its first start.
- **NetworkPolicy lag.** kindnet enforces ingress and egress policy a few seconds after it is applied; OpenShell creates policies before it starts the workload, so this does not affect it.

## The gt-sbx desired set

This section says what the deployer owns on the gt-sbx cluster and what stays in the bootstrap. Each claim names the file or command that shows it; the last subsection says what was not checked. There is no live cluster or chart render in the sandbox this was written in, so the code is the source.

### Apply order

`deploy/k8s/sbx/render.go` (`renderSet`) composes the set in this order:

1. The Namespaces in the pinned agent-sandbox manifest, in manifest order (`deploy/k8s/sbx/agentsandbox.go`).
2. The `openshell` Namespace (`openshellNamespace`).
3. The PriorityClasses from `deploy/k8s/priority-classes.yaml`, through `k8s.SbxStatic` (`deploy/k8s/sbx_static.go`).
4. The other agent-sandbox objects, in manifest order. The controller Deployment is patched here.
5. The OpenShell chart's objects: `helm template` output with the hook documents and the Secrets removed (`deploy/k8s/sbx/openshell.go`). The gateway StatefulSet is re-encoded Guaranteed at 500m CPU and 1Gi memory.
6. The static Services `openshell-nodeport`, `control-plane`, `dolt`, `forgejo` and `tracker`, then the PersistentVolumeClaim `gt-cache` (`sbxStaticOrder` in `render.go`; sources `deploy/k8s/kind/gateway-nodeport.yaml`, `deploy/k8s/values/kind/control-plane-services.yaml` and `deploy/k8s/gt-cache/claim.yaml`).
7. The ConfigMap `gt-bootstrap` in `openshell`, last. Its `helm` key holds the chart version and the SHA-256 of the values file.

The render refuses a Secret in the composed list (`checkNoSecrets`) and two objects with the same kind, namespace and name (`checkDuplicates`).

### Runtime objects the compare ignores

`gt town cluster drift` reports a live object the set does not name, unless `Set.IsRuntime` names it (`internal/cmd/town_cluster_drift_sbx.go`). The ignore lists are `sbxRuntime`, `sbxRuntimeNamespaces` and `sbxRuntimeClusterPrefixes` in `render.go`. Each entry is created by something other than the deployer:

- **The gateway and the daemon:** the NetworkPolicies `openshell-sandbox-supervisors` and `openshell-sandbox-workloads`, and the cache trim Job `gt-cache-trim-gt-cache`.
- **The chart's certgen hook,** which leaves its ServiceAccount, Role and RoleBinding `openshell-certgen` behind. The hook document itself is never applied.
- **kind and kubeadm:** the Namespaces `default`, `kube-system`, `kube-public`, `kube-node-lease` and `local-path-storage`; every object in `kube-system`, `kube-public` and `kube-node-lease`; the ClusterRoles and ClusterRoleBindings with the `system:` and `kubeadm:` prefixes; the bootstrap ClusterRoles `admin`, `edit`, `view` and `cluster-admin`; and kindnet.
- **kind's local-path add-on,** including the StorageClass `standard`.
- **The metrics-server manifest** that `up.sh` applies, including the APIService `v1beta1.metrics.k8s.io`.

The lists were copied by hand from `deploy/k8s/kind/cluster.yaml`, `up.sh` and kind's defaults; the `sbxRuntime` comment says no live cluster was read for them. A missed object shows as an extra in the compare, and the fix is one more row.

### The four Secrets

None of these four is rendered or applied. `Set.Secrets` names them so the existence check can look for them (`Chart.Secrets` in `openshell.go`; `internal/secretpreflight`).

| Secret | Made by |
|---|---|
| `openshell-server-tls` | the certgen hook Job, through `--server-secret-name` |
| `openshell-client-tls` | the certgen hook Job, through `--client-secret-name` |
| `openshell-jwt-keys` | the certgen hook Job, through `--jwt-secret-name` |
| `openshell-credential-storage-key-encryption-key` | the chart's template, with a random value |

The chart's template renders that last Secret with a new random value under `helm template`. Applying it would replace the live key and make the stored credentials unreadable (the `OpenShell` doc comment in `openshell.go`).

### Why helm upgrade is never run

The certgen Job is a `pre-install,pre-upgrade` hook (`deploy/k8s/sbx/testdata/openshell-trimmed.yaml`). Every `helm upgrade` therefore regenerates the CA and every certificate and rolls the gateway, so each exported client certificate goes stale at once, the town registration included (`deploy/k8s/kind/up.sh`, the comment above the helm block).

- The render runs only `helm template` and never `helm upgrade` (`OpenShell` in `openshell.go`).
- `up.sh` is the one place that runs `helm upgrade --install`. It does so only when the `gt-bootstrap` marker differs from the chart version and values digest, when the release is not deployed, or when `GT_K8S_FORCE_HELM=1` is set. An unchanged marker means no helm call (`up.sh`, the helm block).
- The first install stays a helm install inside the rebuild sitting. Plain applies come after it and never call helm (`openshell.go`, the `OpenShell` comment).

### The cluster-per-plan rule

A plan record names the cluster it is for (`planRecord` in `internal/cmd/town_cluster_plan.go`). `gt town cluster apply --cluster` refuses a plan for another cluster, and a plan that names none (`applyClusterGate` in `internal/cmd/town_cluster_apply_cmd.go`). The gate runs before the approval is verified and before any cluster is called.

### The interim read exception

`gt town cluster drift --exec-via docker-exec:<context>:<container>` reads the sandbox cluster's kubectl through `docker exec`, using the kind `admin.conf` inside the node (`internal/cmd/town_cluster_exec.go`). The command prints `ClusterExecViaInterimHeader` before any read. The exception expires at the sandbox cluster's next rebuild, when the read certificate replaces it.

### What stays in the bootstrap

The deployer does not apply these. Each is run by `deploy/k8s/kind/up.sh` or `down.sh`, or is owned by someone else, as the list below says. `SbxExcluded` in `deploy/k8s/sbx_static.go` records the bootstrap files.

- **Cluster create and destroy:** `kind create cluster` (`up.sh`) and `down.sh`.
- **The local registry container,** which runs beside kind (`up.sh`).
- **The containerd `hosts.toml`** for `localhost:<registry port>` on each node (`up.sh`).
- **The metrics-server patch:** `up.sh` applies the pinned manifest and patches its args. Its object names are in the runtime list above.
- **local-path storage:** kind creates it, and it is in the runtime list above.
- **The sandbox policy,** `deploy/k8s/policy/kind.yaml`. The gateway takes it per sandbox create. It is not a cluster object (`StaticExcluded` in `deploy/k8s/static.go`).
- **Sandbox profiles and providers:** created on the gateway through the CLI, not as cluster objects and not in the render. The provider state is in `docs/openshell-gateway.md`. Beads `gt-g08vr` and `gt-ujzas` track the profile and provider work.

`up.sh` also applies the PriorityClasses (`up.sh`, before the metrics-server step), `gateway-nodeport.yaml` and `control-plane-services.yaml` on every run, and the render places the same objects. Bead `gt-qob3po` owns that overlap; this section does not resolve it.

### What was not verified

- No live cluster was read. The runtime lists are copies of the code's lists, not observations.
- No real chart was rendered. `helm` and chart 0.1.4 are not in the sandbox, and the chart fixture is trimmed, so the certgen Secret names come from the fixture, and the random value of the template Secret comes from the render's comment.
- The agent-sandbox manifest's objects were not listed one by one; its bytes must match `AgentSandboxSHA256` before the render uses them.
- The sandbox profiles and providers were checked against the docs and bead titles, not against a gateway.

## Build beads

`gt-s9gqm` this bootstrap; `gt-mm7q6` publish the image to the registry; `gt-g08vr` a `driver` field on sandbox profiles; `gt-ujzas` create providers on a named gateway; `gt-fju1j` the `dolt` and `forgejo` Services and a kind policy. The proof (five beads on the Kubernetes seat) is a seat-config change the overseer makes.
