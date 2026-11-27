# Sandbox rescue runbook: save the work before the sandbox is deleted

Read this when `gt polecat check-recovery` answers NEEDS_RECOVERY for a seat whose sandbox is
failed, suspended, or stays outside Ready (Provisioning included), and before any
`openshell sandbox delete` of it. `openshell sandbox start` refuses an Error sandbox and
`openshell sandbox exec` needs a running pod, so the commits are reached through the workspace
volume on its node. The volume name, node and commit ids below belong to the 2026-11-19 rescue of
gt-cheedo (bead gt-o9qmfe); step 1 reads the seat's own volume and node, and step 3 its commits.

The operator's shell on the Mac runs steps 1 to 4, and has the `colima-gt-sbx` docker context; the PVC copy and bundle stay under `~/gt/.runtime/rescue`. The daemon pod runs what the Mac's `gt` wrapper sends it and holds the `openshell` gateway, so the pod commands in step 5 carry only pod paths. The overseer runs every step; a bird session may run steps 1 to 3, and step 4 only when the overseer says so. Step 5 is the overseer's, after step 4's read-back.

## Why the delete is the trap

`openshell sandbox delete <sandbox>` deletes the Sandbox CR. The PVC `workspace-default--<sandbox>` carries an `ownerReference` of kind `Sandbox` naming `default--<sandbox>`, and its `rancher.io/local-path` PV has reclaim policy `Delete`: garbage collection removes the PVC and the provisioner removes the PV's directory, so every commit that never reached origin goes with it.

Run `gt polecat check-recovery <rig>/<seat>` first: NEEDS_RECOVERY is why this page exists, and the
Polecat section of [CLEANUP.md](CLEANUP.md) says what "landed" means to the verdict. For the example
it is NEEDS_RECOVERY with `has_unpushed` and the branch, since no pod runs to show the commits.

## Before step 1: a sandbox outside Ready that may still run

The gateway composes the sandbox phase from the supervisor pod (`os-supervisor-<sandbox id>`) and
its session as well as from the Sandbox CR, so a Ready CR with a running sandbox pod can still
read Provisioning. Record the sandbox id and the Ready condition's `reason` and `message` from the
JSON, then read the cluster, all before any delete: the delete and log rotation remove the evidence.

```bash
kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- \
  openshell -g k8s sandbox get <sandbox> --output json
kubectl --kubeconfig "$KC" -n openshell get sandbox,pod | grep -E '<sandbox>|os-supervisor-<id>'
```

The first command runs `openshell` in the daemon pod as step 5 does; set `KC` to the kubeconfig
path step 1's block sets. The second read shows the Sandbox CR, the sandbox pod and the supervisor
pod. When a pod runs, step 2 copies a live volume: confirm no process in the pod is writing before
step 2, and say in the rescue record that the copy was live.

## Step 1: find the volume (read-only)

```bash
KC=~/gt/.runtime/sbx-k8s/kubeconfig
kubectl --kubeconfig "$KC" -n openshell get pvc workspace-default--gt-cheedo \
  -o jsonpath='{.metadata.ownerReferences}{"\n"}{.spec.volumeName}{"\n"}'
kubectl --kubeconfig "$KC" get pv pvc-82e8d909-d83b-41df-a20e-4a0bcc1b16dd -o yaml
```

For gt-cheedo the ownerReference names `default--gt-cheedo`, `.spec.volumeName` is `pvc-82e8d909-d83b-41df-a20e-4a0bcc1b16dd`, and the PV's path is `/var/local-path-provisioner/pvc-82e8d909-d83b-41df-a20e-4a0bcc1b16dd_openshell_workspace-default--gt-cheedo` on `gt-sbx-worker2`, the node its affinity names. The volume is `/sandbox` in the sandbox, so its town root is `gt/`.

## Step 2: copy the volume out (read-only on the cluster)

```bash
mkdir -p ~/gt/.runtime/rescue
docker --context colima-gt-sbx cp gt-sbx-worker2:/var/local-path-provisioner/pvc-82e8d909-d83b-41df-a20e-4a0bcc1b16dd_openshell_workspace-default--gt-cheedo \
  ~/gt/.runtime/rescue/gt-cheedo-pvc-copy
du -sh ~/gt/.runtime/rescue/gt-cheedo-pvc-copy
```

gt-cheedo's copy is 130 MB. `docker cp` reads the node's disk, so nothing on the cluster changes;
run no node disk maintenance until the copy completes.

## Step 3: save the commits as a bundle (read-only)

The seat's worktree in the copy is `gt/<rig>/polecats/<name>/<repo>`, here `gt/gastown/polecats/cheedo/gastown`.

```bash
cd ~/gt/.runtime/rescue/gt-cheedo-pvc-copy/gt/gastown/polecats/cheedo/gastown
git log --oneline origin/main..HEAD    # 5137f61d and 95b0512c, on polecat/cheedo/gt-o9qmfe+mwo83624
git bundle create ~/gt/.runtime/rescue/cheedo-gt-o9qmfe-95b0512c.bundle origin/main..HEAD
git bundle verify ~/gt/.runtime/rescue/cheedo-gt-o9qmfe-95b0512c.bundle
```

`git bundle verify` must answer okay before step 4, one ref at 95b0512c requiring a8f9da44, because
the bundle is the copy that survives the delete.

## Step 4: put the tip on origin (write step)

This step writes, creating a rescue ref on origin: the overseer runs it from the overseer's clone, or a bird session only when the overseer says so.

```bash
cd <overseer-clone>
git fetch ~/gt/.runtime/rescue/cheedo-gt-o9qmfe-95b0512c.bundle HEAD
git worktree add -b polecat/cheedo/gt-o9qmfe+mwo83624-95b0512 \
  ~/gt/gastown/crew/overseer-rescue FETCH_HEAD
cd ~/gt/gastown/crew/overseer-rescue
git push origin HEAD
git ls-remote origin 'refs/heads/polecat/cheedo/gt-o9qmfe*'
git worktree remove ~/gt/gastown/crew/overseer-rescue
```

The push uses the branch's own name when origin does not hold it; gt-cheedo's name already existed on origin at 00f1095c, a different commit, so the push created a new branch carrying the `-<sha7>` suffix the nuke's fallback uses. The read-back must print 95b0512c on the rescue ref; on 2026-11-19 `git ls-remote` read both refs back, the original at 00f1095c and the rescue ref at 95b0512c. `preserveBranchBeforeNuke` and `unpreservedBranchRefusal` in `internal/cmd/polecat.go` guard the nuke: a branch origin already holds at another commit is parked on `<branch>-<sha7>`, and work no remote ref holds is a refusal to delete, not a silent loss. The pre-push guard (`.githooks/pre-push`, explained by `preserveCheckout` in `internal/git/preserve.go`) refuses a `refs/heads` push from a checkout on another branch, so the rescue branch is checked out in its own worktree and pushed as `HEAD`: the guard sees the branch it is told to push, and nothing is pushed around it. The earlier nukes of dementus and rictus parked `polecat/dementus/gt-d0glu1+mwnrm11d-301dec4` and `polecat/rictus/gt-9p2wh7+mwo2agyv-1dbca18` on origin the same way.

Hand the nuke no local ref and no `--force`: step 4 is the only push this runbook makes, and the nuke's own preserve step finds the tip on origin. Its `--force` with `--acknowledge-unpreserved` is for a branch whose only copy is local, after a patch-id proof that origin holds the same change, as the operator handbook `gastown-operations.md` says; it is not a way past the `has_unpushed` verdict that sent you here.

## Step 5: delete the sandbox, then nuke the seat (destructive)

Run this only after `git bundle verify` exited 0 and the read-back printed the tip.

```bash
kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- \
  openshell -g k8s sandbox delete gt-cheedo
gt polecat nuke gastown/cheedo --dry-run
gt polecat nuke gastown/cheedo
```

The delete answered `Deleted sandbox gt-cheedo` and takes the volume through the PVC's ownerReference and the PV's `Delete` policy. The plain nuke is the `gt` wrapper's command in the pod; the dry-run read Verdict clear (hook empty), and the nuke's preserve step confirmed origin held the tip, answering `already reachable from origin/crew/merlin/gt-3jad9w, nothing to preserve` and `Nuked 1 polecat(s)`.

## A delete refused as pending

Read this when `openshell sandbox delete <name>` answers `previous driver operation is still pending; retry and deletion are blocked`, the sandbox is in phase Error, `kubectl get sandbox default--<name>` answers NotFound, and no PVC or pod holds the name. If a PVC or pod exists, follow the steps above instead.

The gateway restarted between dispatch and the Sandbox CR, which left the sandbox row in phase Error with no CR. Its driver operation record has no expiry, so the gateway retries it about every 2.6 s. `sandbox delete` has no force flag, so the refusal holds while that record stands, and waiting does not clear it.

The clear deletes three rows from the gateway's sqlite store with the gateway scaled to zero: every live seat loses its gateway link for about a minute and reconnects. It is the operator's attended step, opened by the overseer, who says so first. The steps are the gt-2r7agv OPERATOR CLEAR for the `gt-dag` row of 2026-11-22 (sandbox id `3c588be9-0ef1-4185-b03e-9d63c6490f33`); a later seat re-reads its own id, PV path and rows before any step.

```bash
KC='docker --context colima-gt-sbx exec gt-sbx-control-plane kubectl --kubeconfig /etc/kubernetes/admin.conf'
D=/var/local-path-provisioner/pvc-49defbc2-a3f2-415c-8042-55697f1c40d7_openshell_openshell-data-openshell-0
W=~/gt/.runtime/rescue/gw-fix-$(date +%Y%m%d%H%M)
```

The store is sqlite in `$D` on node `gt-sbx-worker` (`openshell.db`, `-wal`, `-shm`; owner 1000, mode 600). The gateway image has no shell or sqlite3, so the edit runs on a Mac copy.

0. **Window.** Confirm the dispatch hold is on and no seat is mid-landing. Tell nux, rictus and dv-anchor (and any later seat) their gateway link drops for about a minute. Continue only on the overseer's word.
1. **Pre-checks.** Each must print what is shown; any other row, or a CR, PVC or pod for `gt-dag`, is a STOP.

   ```bash
   $KC -n openshell get sandbox default--gt-dag    # NotFound
   $KC -n openshell get pvc,pod | grep -c gt-dag    # 0
   docker --context colima-gt-sbx cp gt-sbx-worker:$D/. $W/
   sqlite3 -readonly $W/openshell.db "select object_type,id from objects where id like '%3c588be9-0ef1-4185-b03e-9d63c6490f33%' or (object_type='sandbox_policy' and scope='3c588be9-0ef1-4185-b03e-9d63c6490f33')"
   ```

   The query prints exactly three rows: `sandbox|3c588be9-...`, `sandbox_ssh_identity_v1|sandbox-ssh:3c588be9-...` and `sandbox_policy|07c81916-b29a-4db0-9b2f-f8157be8b1ea`.
2. **Backup.** `cp -R $W $W.orig`; keep both until the next day.
3. **Stop the gateway.** `$KC -n openshell scale statefulset openshell --replicas=0`; continue when `$KC -n openshell get pod openshell-0` answers NotFound.
4. **Final copy** (the gateway is down, so the files are consistent): `rm -rf $W && mkdir $W && docker --context colima-gt-sbx cp gt-sbx-worker:$D/. $W/`
5. **Edit the copy.**

   ```bash
   sqlite3 $W/openshell.db "begin; delete from objects where object_type='sandbox' and id='3c588be9-0ef1-4185-b03e-9d63c6490f33' and name='gt-dag'; delete from objects where object_type='sandbox_ssh_identity_v1' and id='sandbox-ssh:3c588be9-0ef1-4185-b03e-9d63c6490f33'; delete from objects where object_type='sandbox_policy' and scope='3c588be9-0ef1-4185-b03e-9d63c6490f33'; select changes(); commit; pragma wal_checkpoint(truncate); pragma integrity_check;"
   sqlite3 -readonly $W/openshell.db "select count(*) from objects where name='gt-dag' or id like '%3c588be9%' or scope='3c588be9-0ef1-4185-b03e-9d63c6490f33'"
   sqlite3 -readonly $W/openshell.db "select count(*) from objects where object_type='sandbox'"
   ```

   Each delete removes one row and `integrity_check` prints `ok`. The first count reads 0; the last reads one fewer than in step 1.
6. **Put the copy back.**

   ```bash
   docker --context colima-gt-sbx exec gt-sbx-worker rm -f $D/openshell.db-wal $D/openshell.db-shm
   docker --context colima-gt-sbx cp $W/openshell.db gt-sbx-worker:$D/openshell.db
   docker --context colima-gt-sbx exec gt-sbx-worker chown 1000:0 $D/openshell.db
   docker --context colima-gt-sbx exec gt-sbx-worker chmod 600 $D/openshell.db
   docker --context colima-gt-sbx exec gt-sbx-worker ls -ln $D
   ```

   The listing shows one file, owner 1000, mode `-rw-------`.
7. **Start the gateway.** `$KC -n openshell scale statefulset openshell --replicas=1`; continue when `openshell-0` reads `1/1 Running`.
8. **Verify.** `kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- openshell -g k8s sandbox list` shows no `gt-dag` and every running seat Ready within two minutes; the gateway log, from its previous line count, shows no `reclamation` retries. Then the overseer lifts the hold, and the first sling to the name `dag` (a trial bead) must reach Ready.

**Rollback:** scale the gateway to 0, remove the node's `-wal` and `-shm`, copy `$W.orig/.` back to `$D` (the original files), set owner 1000:0, and scale to 1. The `gt-dag` row returns as it was.

## Not verified

- With no pod, the copy is taken from the node's disk while the pod is gone, and nothing writes the volume mid-copy; for a running pod the one case seen (gt-cheedo, 2026-11-26) was copied with the agent idle and read a clean worktree, and stopping the sandbox through a stale gateway was not tried.
- The PV name, node, path, size and commits are gt-cheedo's readings; re-read each for the next seat.
- Whether the daemon pod is allowed to push the branch its nuke preserves; a refused push stops the nuke with the branch and verified bundle intact.
- Step 4 is one hand push because the nuke's preserve step reads only the seat's own worktree, which holds none of the commits copied out of the volume (`gt polecat nuke --help`: it "pushes the polecat's branch"). Whether the copied commits could instead be fetched into that worktree's branch, so the plain nuke pushes them and no second push path exists, has not been tried: the worktree sits in the daemon pod's town copy while the bundle sits on the Mac, and the nuke deletes it.
