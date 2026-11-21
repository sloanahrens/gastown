# Sandbox rescue runbook: save the work before the sandbox is deleted

Read this when `gt polecat check-recovery` answers NEEDS_RECOVERY for a seat whose sandbox is
failed or suspended, and before any `openshell sandbox delete` of it. `openshell sandbox start`
refuses an Error sandbox and `openshell sandbox exec` needs a running pod, so the commits are
reached through the workspace volume on its node. The steps below are the gt-cheedo rescue of
2026-11-19 (bead gt-o9qmfe), in the order it ran; a later seat re-reads its own names.

The operator's shell on the Mac runs steps 1 to 4, and has the `colima-gt-sbx` docker context; the PVC copy and bundle stay under `~/gt/.runtime/rescue`. The daemon pod runs what the Mac's `gt` wrapper sends it and holds the `openshell` gateway, so the pod commands in step 5 carry only pod paths. The overseer runs every step; a bird session may run steps 1 to 3, and step 4 only when the overseer says so. Step 5 is the overseer's, after step 4's read-back.

## Why the delete is the trap

`openshell sandbox delete <sandbox>` deletes the Sandbox CR. The PVC `workspace-default--<sandbox>` carries an `ownerReference` of kind `Sandbox` naming `default--<sandbox>`, and its `rancher.io/local-path` PV has reclaim policy `Delete`: garbage collection removes the PVC and the provisioner removes the PV's directory, so every commit that never reached origin goes with it.

Run `gt polecat check-recovery <rig>/<seat>` first: NEEDS_RECOVERY is why this page exists, and the
Polecat section of [CLEANUP.md](CLEANUP.md) says what "landed" means to the verdict. For the example
it is NEEDS_RECOVERY with `has_unpushed` and the branch, since no pod runs to show the commits.

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
git fetch ~/gt/.runtime/rescue/cheedo-gt-o9qmfe-95b0512c.bundle HEAD:refs/rescue/cheedo-o9qmfe
git push origin \
  refs/rescue/cheedo-o9qmfe:refs/heads/polecat/cheedo/gt-o9qmfe+mwo83624-95b0512
git ls-remote origin 'refs/heads/polecat/cheedo/gt-o9qmfe*'
```

The push uses the branch's own name when origin does not hold it; gt-cheedo's name already existed on origin at 00f1095c, a different commit, so the push created a new branch carrying the `-<sha7>` suffix the nuke's fallback uses. The read-back must print 95b0512c on the rescue ref; on 2026-11-19 `git ls-remote` read both refs back, the original at 00f1095c and the rescue ref at 95b0512c. `preserveBranchBeforeNuke` and `unpreservedBranchRefusal` in `internal/cmd/polecat.go` guard the nuke: a branch origin already holds at another commit is parked on `<branch>-<sha7>`, and work no remote ref holds is a refusal to delete, not a silent loss. The pre-push guard (`.githooks/pre-push`, explained by `preserveCheckout` in `internal/git/preserve.go`) refuses a `refs/heads` push from a checkout on another branch, so the rescue push reads from `refs/rescue/`. The earlier nukes of dementus and rictus parked `polecat/dementus/gt-d0glu1+mwnrm11d-301dec4` and `polecat/rictus/gt-9p2wh7+mwo2agyv-1dbca18` on origin the same way.

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

## Not verified

- The copy is taken from the node's disk while the pod is gone; a running pod needs the sandbox stopped first, so nothing writes the volume mid-copy.
- The PV name, node, path, size and commits are gt-cheedo's readings; re-read each for the next seat.
- Whether the daemon pod is allowed to push the branch its nuke preserves; a refused push stops the nuke with the branch and verified bundle intact.
