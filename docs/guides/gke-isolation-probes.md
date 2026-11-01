> Status: current (2026-11). Runbook for plan row S3; the live run is owed by the operator in a grant window and has not run.

# Isolation probes on a GKE Standard Ubuntu node

Read this before a hosted workspace is scheduled onto a GKE Standard node, and before any two workspaces share one. It runs the OpenShell supervisor's three isolation probes on that node and records the answer on the map ticket claude-29a.13. Row S3 of the hosted-platform plan (the plan document in the claude repo). Before registering the gateway the probes run through, read [openshell-gateway.md](../openshell-gateway.md); this page is the probe, not the setup.

The property this run keeps: a sandbox cannot read another sandbox's or the host's memory or files. The result decides whether co-tenant workspaces may share a node at all. No outside customer is onboarded until the result is recorded on claude-29a.13.

The probes are the supervisor's own: `openshell-sandbox` runs them while a sandbox starts and refuses the workload when one fails, so this page adds no probe of its own. `openshell doctor check` is not one of them: it checks that the container runtime answers, and it passes on a node whose kernel lacks Landlock (NVIDIA/OpenShell issue 4133).

## Before the window

- S2's preview deploy is clean, the operator has said go and the grant window is open. Those are the row's wait conditions.
- The pool is GKE Standard with the Ubuntu containerd image and AppArmor on, which is GKE's Ubuntu default, and it is created for this window with Spot nodes (the cost line assumes it). The run asserts the node the sandbox lands on, so the pool size does not have to prove it. Landlock is compiled out of Container-Optimized OS, so confirm the image before starting (`$NODE` is the probe pool's node):

```bash
kubectl get nodes -o wide
kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.osImage}{"\n"}'
```

A line starting `Ubuntu` passes this check; `Container-Optimized OS` stops the run.

- The cluster's gateway is registered and healthy: `openshell -g "$GATEWAY" status`. `$GATEWAY`, `$NODE`, `$NS` (the namespace the sandbox runs in), `$DATE`, `$PROJECT`, `$POOL`, `$CLUSTER` and `$LOCATION` are set in the window and recorded nowhere else.

## The run

One sandbox start runs all three probes. The supervisor admits the workload only after every probe passes, so a sandbox that is admitted and `Ready` is the only positive evidence this page can use, and a failed probe exits non-zero and names the check. Capture the exit status, the sandbox list, the node the sandbox's pod landed on and the supervisor log, each in its own file:

```bash
DIR=~/gt/.runtime/gke-isolation/run-$DATE
mkdir -p "$DIR"
openshell -g "$GATEWAY" sandbox create --name isolation-probe \
  --no-auto-providers --no-tty -- echo "sandbox ready" > "$DIR/create.txt" 2>&1
echo $? > "$DIR/create.status"
openshell -g "$GATEWAY" sandbox list > "$DIR/list.txt" 2>&1
kubectl get pods -n "$NS" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}' \
  | grep -F isolation-probe > "$DIR/placement.txt"
openshell -g "$GATEWAY" logs isolation-probe -n 5000 > "$DIR/log.txt" 2>&1
```

Nothing in this block is required to print: an empty file is read as a failure by the verdict below. The log window is 5000 lines; a log that fills the window may have lost its start, and the verdict fails closed on it.

## The verdict

The verdict is computed, not judged, and it claims less than a pass. No probe is known to print a positive success line, so none can be shown to have run and passed: the result for a probe is `no-known-failure` (the sandbox was admitted and `Ready`, on the node checked, and the log holds none of that probe's known failure messages) or `fail`. This holds for all three probes; the Landlock probe alone sometimes logs its ABI, and that is an extra reading, not a required one. A missing, empty, unreadable or window-filling input makes every probe `fail`. The conditions are that `create` exited 0, the list shows `isolation-probe` as exactly `Ready` (not `Not Ready` or `NotReady`), the pod's node is `$NODE`, and the log is non-empty and shorter than the 5000-line window. The failure patterns are the supervisor's own messages, one set per probe:

| Probe | Failure pattern (extended regex) |
|---|---|
| Landlock | `Landlock probe child exited with status 1`, `incompatible access-rights`, or an `ABI` below 3 |
| seccomp user-notify | `notification launcher disappeared`, `ControlSupervisorStartFailed` |
| task-memory (`process_vm_readv`) | `task-memory` or `process_vm_readv` on a line with `fail` or `denied` |

Below Linux 5.19 seccomp falls back to the legacy read-only listener; that is still a pass, with mediated memory writes failing closed. `process_vm_readv` or the `/proc/<pid>/mem` fallback satisfies the task-memory probe.

```bash
verdict() {  # verdict DIR NODE: prints one line per probe, then the VERDICT line
  local d=$1 node=$2 ok=1 failed=""
  [ "$(cat "$d/create.status" 2>/dev/null)" = 0 ] || ok=0
  awk '$1 == "isolation-probe" { for (i = 2; i <= NF; i++) if ($i == "Ready" && $(i-1) != "Not") f = 1 } END { exit !f }' "$d/list.txt" 2>/dev/null || ok=0
  awk -v n="$node" '$2 == n { f = 1 } END { exit !f }' "$d/placement.txt" 2>/dev/null || ok=0
  [ -s "$d/log.txt" ] && [ "$(wc -l < "$d/log.txt")" -lt 5000 ] || ok=0
  local name pat
  for name in landlock seccomp-user-notify task-memory; do
    case $name in
      landlock) pat='Landlock probe child exited with status 1|incompatible access-rights|ABI[ =:]*[0-2]([^0-9]|$)' ;;
      seccomp-user-notify) pat='notification launcher disappeared|ControlSupervisorStartFailed' ;;
      task-memory) pat='(task-memory|process_vm_readv).*(fail|denied)' ;;
    esac
    if [ "$ok" = 1 ] && ! grep -qiE "$pat" "$d/log.txt"; then
      echo "probe=$name result=no-known-failure"
    else
      echo "probe=$name result=fail"; failed="$failed${failed:+,}$name"
    fi
  done
  if [ -z "$failed" ]; then echo "VERDICT NO-KNOWN-FAILURE"; else echo "VERDICT FAIL probe=$failed"; fi
}
verdict "$DIR" "$NODE" | tee "$DIR/verdict.txt"
```

`VERDICT NO-KNOWN-FAILURE` is never to be read as proven isolation. Only a person who reads `log.txt` and the node's kernel and image against the supervisor's documentation can say more, and the comment on claude-29a.13 says whether that was done. Before relying on `verdict()`, run it against sample directories: a clean one, one per failure pattern, a `Not Ready` list, another node's placement, a missing `create.status`, an empty log and a 5000-line log. This page records no result of such a run.

## Where the result is recorded

1. `$DIR` keeps the create output, the list, the placement, the supervisor log and `verdict.txt`, so an agent can read the run back.
2. Paste `verdict.txt` (the three probe lines and the verdict line) into a comment on the map ticket claude-29a.13, with `$DIR`'s path and the node image and kernel the run used. That comment is the record; the row reads no other place.
3. A `VERDICT FAIL` keeps co-tenancy off until a later run gives `NO-KNOWN-FAILURE`. No outside customer is onboarded, and no two workspaces share a node, until claude-29a.13 carries that line, a statement of whether the log was read by a person, and the node image and kernel.

## Teardown

```bash
openshell -g "$GATEWAY" sandbox delete isolation-probe
gcloud config get-value project   # must print $PROJECT; stop if it does not
gcloud container node-pools delete "$POOL" --project "$PROJECT" --cluster "$CLUSTER" --location "$LOCATION"
```

`$POOL` is the probe pool the window created (set it where `$NODE` is set); delete only that pool. Nothing else in the cluster changes.

## Cloud-change review answers

**Blast radius.** One single-node probe pool in the operator's project, and the sandboxes on it. A failed probe kills only that sandbox; no customer workspace, production pool or secret is touched, and the run writes one results file.

**Cost line.** One Spot node plus the cluster fee for the length of the window, and nothing after teardown. Treat that as an estimate to check on the GKE pricing page, not a figure from this page.

**Undo.** The Teardown section. The results file and the claude-29a.13 comment stay as the record of what ran.

**Who holds the keys.** The operator holds the GCP project and runs every command in the grant window. The probes need no credential and the sandbox holds no customer secret; no key value, project number or token appears in this file.

## The check this file passes

Each name the row requires is present, so this prints one line per probe and one for the recording target:

```bash
grep -nE 'Landlock|seccomp user-notify|process_vm_readv|claude-29a\.13' docs/guides/gke-isolation-probes.md
```

The repository's own check is expected to exit 0 with no finding:

```bash
go run ./cmd/docs-lint
```
