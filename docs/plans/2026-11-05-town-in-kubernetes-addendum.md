> Status: historical (2026-11). Addendum to: `docs/plans/2026-11-04-town-in-kubernetes-design.md` and `docs/plans/2026-11-04-town-in-kubernetes-plan.md`, both frozen; where this file and either of them disagree, this file wins. Not maintained.

# The town in Kubernetes: addendum (2026-11-05)

The 2026-11-04 design and plan are frozen as written. This addendum records what the operator decided after them and the state after update cycle 2. Every part of the frozen files not named below still stands.

## 1. D5 revised: the control cluster stays k3s (2026-11-05)

The operator decided D5 again on 2026-11-05:

- The control cluster stays k3s on the `gt-control` Colima VM.
- P2.1 and P2.2 (the kind rebuild) are dropped.
- kind returns only if a second node pool is needed, and then the first step is to add a k3s node, not to rebuild the cluster as kind.

These frozen lines are wrong and are corrected here:

- Design §2, the D5 row, including the phrase "kind everywhere" and the sentence "`gt-control` is rebuilt as a kind cluster with core and privileged pools": k3s stays, and no kind rebuild is planned.
- Design §5, the options row "k3s for the control cluster", which says "replaced by kind": k3s is kept; the control cluster is not rebuilt.
- Plan §Phase 2, the heading "the control cluster becomes kind" and rows P2.1 and P2.2: both rows are dropped.

## 2. Phase order accepted (2026-11-05)

The accepted order is: container gaps, the registry, CI images, hm in parallel, the daemon into the cluster, kind last. "kind last" is the conditional rebuild of section 1; it starts only if a second node pool is needed.

## 3. State after update cycle 2 (2026-11-05, 13:58 CST)

Update cycle 2 left the daemon image, the sandbox image and the Mac `gt` all at `cf0d2c17b`.

Live on main: gt-gg9hv.24.1 to gt-gg9hv.24.4 (disk maintenance reads the container's clusters and disks and trims the Go cache on its own interval), gt-ppgga.1 (the host agent's daily node image prune) and gt-hlo07.1 (tier-sweep Jobs mount a persistent per-rig Go cache claim).

Forgejo and its runner run in the k3s namespaces `forgejo` and `forgejo-trial`.

The tier-sweep route is staged: namespace `gt-sweeps` with a scoped ServiceAccount and Role, a `colima-gt-control` context in the daemon container, and the registry forwarder (gt-gg9hv.25) with no credentials, which lets the sweep name the sandbox image instead of loading it into the Mac's engine (gt-gg9hv.26).

## 4. P3.5 retires only the old agent

P3.5's "retire the host agent" means only the OLD agent that ran town patrols and plugins. The host agent is the small local Mac job for Mac-only read, report and maintenance work. Nobody removes it, and the town never depends on it: with no agent installed the registry collect reports "collect skipped" and disk maintenance reports "host disks unknown", never an error.

The frozen lines to read this way are Design §3's "Retired with the move" list, which names "the host agent", and Plan §Phase 3, row P3.5.

## 5. The kind control-cluster files

The control-cluster kind config and runbook under `deploy/k8s/control` are removed by the sibling bead gt-gg9hv.29, and git history holds them. The path named in P2.1 is history, not a live file.
