# GKE probe

`probe.sh` answers one question for the cloud track: does OpenShell's sandbox
boundary run on a GKE Standard node with the Ubuntu image? Landlock is compiled
out of Container-Optimized OS (so GKE Autopilot), and in on Ubuntu kernels.

It is a wizard for a human. It creates one Spot node in a GCP project and bills
until it is deleted, so agents never run it.

```bash
DRY_RUN=1 bash deploy/gke-probe/probe.sh   # print every command, run none
bash deploy/gke-probe/probe.sh             # asks before each billable step
bash deploy/gke-probe/probe.sh teardown    # delete leftover probe clusters
```

It reads the node's kernel and OS image from the API, then reads the active LSM
list and boot line over `gcloud compute ssh` — no pod mounts the node, so
nothing in the cluster gets a root hostPath. It then runs a capability-free pod
that reports the Landlock ABI. OpenShell needs ABI 3 or newer. Results go to
`~/gt/.runtime/gke-probe/results-<date>.txt` with a `VERDICT PASS` or
`VERDICT FAIL` line; a probe pod that fails or prints nothing records a FAIL,
never a missing verdict.

Anything but a clear pass is a `VERDICT FAIL`, because the point of the run is
to prove the boundary works. If the run dies after the cluster exists, it
deletes the cluster on the way out; `GT_PROBE_KEEP=1` keeps it for debugging,
and the leftover line names the teardown command.

Cost is an estimate to check on the GKE pricing page: a few cents an hour for
one Spot node plus the cluster fee, which a free-tier credit may cover. Every
resource is labelled `gt-purpose=openshell-probe` and `gt-ttl` (epoch seconds,
four hours out). Teardown reads the project with `gcloud projects describe`,
prints it, asks before deleting anything in it, and deletes only clusters
carrying that label; set `GT_PROBE_PROJECT` to point it at another project.
Both zonal and regional leftovers are handled.

A pass does not prove the whole chain. It does not test seccomp ADDFD under the
node's runtime or the OpenShell chart; those come next.

Test: `bash deploy/gke-probe/probe_test.sh` (stub gcloud and kubectl, no cloud).
