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

It reads the node's kernel, active security modules and boot line, then runs a
capability-free pod that reports the Landlock ABI. OpenShell needs ABI 3 or
newer. Results go to `~/gt/.runtime/gke-probe/results-<date>.txt` with a
`VERDICT PASS` or `VERDICT FAIL` line.

Cost is an estimate to check on the GKE pricing page: a few cents an hour for
one Spot node plus the cluster fee, which a free-tier credit may cover. Every
resource is labelled `gt-purpose=openshell-probe` and `gt-ttl` (epoch seconds,
four hours out); teardown deletes only clusters with that label.

A pass does not prove the whole chain. It does not test seccomp ADDFD under the
node's runtime or the OpenShell chart; those come next.

Test: `bash deploy/gke-probe/probe_test.sh` (stub gcloud and kubectl, no cloud).
