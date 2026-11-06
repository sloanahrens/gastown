# Control cluster disk pressure

Read this when the control node reports disk pressure, when Forgejo or the CI
runner stops rescheduling, or before changing a toleration on either. It holds
the decision on what tolerates the `node.kubernetes.io/disk-pressure` taint and
why. The patch text is `deploy/k8s/forgejo-tolerations.yaml`; the runner
builder is `deploy/k8s/forgejo-runner`.

## The incident

- 2026-11-05 23:09-23:18 CST: the control node reached 91% disk and the kubelet
  set `node.kubernetes.io/disk-pressure:NoSchedule`.
- The kubelet evicted Forgejo, forgejo-trial and the CI runner. None could
  reschedule: the event read untolerated taint.
- The overseer freed disk by hand, which cleared the taint.
- The largest single consumer was the runner's Go build cache, `ci-gocache`, at
  61 GB.

## The decision

**(a) Forgejo tolerates the taint.** The two Forgejo Deployments, in `forgejo`
and `forgejo-trial`, tolerate `node.kubernetes.io/disk-pressure` with operator
`Exists` and effect `NoSchedule`, and no `tolerationSeconds`. Forgejo is the
service every agent and the daemon depend on. A single-node control cluster has
no other node to place it on, so refusing the taint would leave it down for as
long as the pressure lasts.

**(b) The runner does not tolerate the taint.** The runner's dind store is the
writer that filled the disk. A runner held Pending under pressure stops the
filling. `TestRunnerDoesNotTolerateDiskPressure` keeps the rendered runner
Deployment free of the toleration.

**(c) The standing cap on the runner's store is the age trim and the disk
alert.** The claim's 40Gi is a request, and local-path storage does not enforce
it. The age trim of the Go build cache (gt-gg9hv.57) is meant to bound what the
store holds, and the disk alert (a sibling bead) warns before pressure starts.

A toleration lets an evicted pod be scheduled again. It does not stop the
kubelet from evicting it again while the pressure stays. That is why (c)
matters.

## Thresholds to know

- Eviction begins at about 85% to 90% disk used on this node.
- The disk alert fires at 80%.

These are the figures observed on the one control node. They are not a kubelet
setting this repo writes.

## Applying the toleration

The overseer applies `deploy/k8s/forgejo-tolerations.yaml`. The Forgejo
Deployments live outside every repo. The apply restarts each Deployment, a brief
Recreate outage for each.

Tolerations have no merge key, so a strategic-merge patch replaces the pod's
whole toleration list. Read the live list first. If it holds any toleration other
than this one, the patch must carry those too.

## Not verified

These are open. Nothing in the repo settles them yet.

- Whether a toleration alone ends repeat evictions under sustained pressure.
  Expect the kubelet to evict Forgejo again if the disk stays above the eviction
  threshold.
- Whether ephemeral-storage requests on the Forgejo pods would rank them lower
  for eviction.

The next pressure event answers both. Capture `kubectl describe node` for the
control node and the eviction events for the Forgejo pods, and record the
result here.
