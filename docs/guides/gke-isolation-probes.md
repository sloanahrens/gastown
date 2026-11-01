> Status: current (2026-11). Runbook for plan row S3; the live run is owed by the operator in a grant window and has not run.

# Isolation probes on a GKE Standard Ubuntu node

Read this before a hosted workspace is scheduled onto a GKE Standard node, and before any two workspaces share one. It runs the OpenShell supervisor's three isolation probes on that node and records the answer on the map ticket claude-29a.13. Row S3 of the hosted-platform plan (the plan document in the claude repo). Before registering the gateway the probes run through, read [openshell-gateway.md](../openshell-gateway.md); this page is the probe, not the setup.

The property this run keeps: a sandbox cannot read another sandbox's or the host's memory or files. The result decides whether co-tenant workspaces may share a node at all. No outside customer is onboarded until the result is recorded on claude-29a.13.

The probes are the supervisor's own: `openshell-sandbox` runs them while a sandbox starts and refuses the workload when one fails, so this page adds no probe of its own. `openshell doctor check` is not one of them: it checks that the container runtime answers, and it passes on a node whose kernel lacks Landlock (NVIDIA/OpenShell issue 4133).

## Before the window

- S2's preview deploy is clean, the operator has said go and the grant window is open. Those are the row's wait conditions.
- The pool is GKE Standard with the Ubuntu containerd image and AppArmor on, which is GKE's Ubuntu default, and it holds one node so the sandbox provably lands on the node checked here. Landlock is compiled out of Container-Optimized OS, so confirm the image before starting:

```bash
kubectl get nodes -o wide
kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.osImage}{"\n"}'
```

A line starting `Ubuntu` passes this check; `Container-Optimized OS` stops the run.

- The cluster's gateway is registered and healthy: `openshell -g "$GATEWAY" status`. `$GATEWAY`, `$NODE` and `$DATE` are set in the window and recorded nowhere else.

## The run

One sandbox start runs all three probes. The supervisor admits the workload only after every probe passes, so `create` exiting 0 with the sandbox `Ready` is the pass for all three; a failed probe exits non-zero and names the check. Capture the command's output, the sandbox's phase and the supervisor log:

```bash
OUT=~/gt/.runtime/gke-isolation/results-$DATE.txt
mkdir -p "$(dirname "$OUT")"
{
  echo "# isolation probes $DATE gateway=$GATEWAY node=$NODE"
  openshell -g "$GATEWAY" sandbox create --name isolation-probe \
    --no-auto-providers --no-tty -- echo "sandbox ready"
} 2>&1 | tee "$OUT"
openshell -g "$GATEWAY" sandbox list | grep isolation-probe | tee -a "$OUT"
openshell -g "$GATEWAY" logs isolation-probe -n 200 | tee -a "$OUT"
```

Each probe's line is read from `$OUT` by its own command, and recorded whether it prints or not:

| Probe | Read its line | Pass | Fail |
|---|---|---|---|
| Landlock | `grep -niE 'landlock\|abi' "$OUT"` | admitted, the sandbox is `Ready`, and no Landlock failure is logged (the log names ABI 3 or newer when it logs one) | `Landlock probe child exited with status 1` or `incompatible access-rights`, or an ABI below 3 |
| seccomp user-notify | `grep -niE 'seccomp\|notification\|listener' "$OUT"` | admitted, the sandbox is `Ready`, and no seccomp failure is logged; below Linux 5.19 it falls back to the legacy read-only listener, still a pass, with mediated memory writes failing closed | `seccomp notification probe: notification launcher disappeared` or `ControlSupervisorStartFailed` |
| task-memory (`process_vm_readv`) | `grep -niE 'task.?memory\|vm_readv\|/proc/.*/mem' "$OUT"` | admitted, the sandbox is `Ready`, and no task-memory failure is logged; `process_vm_readv` or the `/proc/<pid>/mem` fallback satisfies it | the log names the task-memory check as failed |

## The verdict

Append one line to `$OUT`: `VERDICT PASS` only when `create` exited 0, the sandbox is `Ready` and none of the three failure lines is present. Any other outcome is `VERDICT FAIL probe=<landlock|seccomp-user-notify|task-memory>`: a non-zero exit, a sandbox that is not `Ready`, a failure line, or a log that could not be read. A missing pass line is not a pass on its own; record it as `(no line)` and rely on the supervisor's admit, never on a grep that printed nothing.

## Where the result is recorded

1. `$OUT` keeps the create output, the supervisor log and the verdict line, so an agent can read the run back.
2. Paste the verdict line and the three probe lines into a comment on the map ticket claude-29a.13, with `$OUT`'s path. That comment is the record; the row reads no other place.
3. A FAIL keeps co-tenancy off. No outside customer is onboarded, and no two workspaces share a node, until claude-29a.13 carries a PASS naming the node image and kernel the run used.

## Teardown

```bash
openshell -g "$GATEWAY" sandbox delete isolation-probe
```

Delete a probe pool the window created too; nothing else in the cluster changes.

## Cloud-change review answers

**Blast radius.** One single-node probe pool in the operator's project, and the sandboxes on it. A failed probe kills only that sandbox; no customer workspace, production pool or secret is touched, and the run writes one results file.

**Cost line.** One Spot node plus the cluster fee for the length of the window, and nothing after teardown. Treat that as an estimate to check on the GKE pricing page, not a figure from this page.

**Undo.** Delete the sandboxes, then the pool the window created. The results file and the claude-29a.13 comment stay as the record of what ran.

**Who holds the keys.** The operator holds the GCP project and runs every command in the grant window. The probes need no credential and the sandbox holds no customer secret; no key value, project number or token appears in this file.

## The check this file passes

Each name the row requires is present, so this prints one line per probe and one for the recording target:

```bash
grep -nE 'Landlock|seccomp user-notify|process_vm_readv|claude-29a\.13' docs/guides/gke-isolation-probes.md
```

The repository's own check exits 0 with no finding:

```bash
go run ./cmd/docs-lint
```
