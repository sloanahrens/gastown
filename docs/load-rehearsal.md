# The load rehearsal runbook: before, run and stop

Read this before the overseer runs one load rehearsal. It is the procedure for the scenario the operator ruled on 2026-11-26: a rehearsal rig seeded from gastown at one tagged commit, its own beads database and gate, a fixed set of sixteen beads released three in flight, and three Mac presubmits of the tag on empty Go caches beside them. One run writes one run record, and Restore at the end puts the town back.

`gt-rehearsal run` drives one run; the flags below are the ones it reads.

## Before

Run every step from the town root, `~/gt`: the driver reads today's host load file and the rehearsal set from paths below it. A step that fails starts no run; Restore still runs for the hold or cap this run already wrote.

1. **No polecat is live.** `gt polecat list --all` names no polecat working, and the `landing` field in `gt status` reads `0 pending`. A live seat or a landing in flight changes the load the run measures.
2. **Automatic dispatch is held.** Run `gt daemon status`. With its `Dispatch hold` line reading `clear`, write this run's hold where the dispatcher reads it:
   ```bash
   kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- \
     sh -c 'printf "%s\n" "overseer: load rehearsal; automatic dispatch held" > seat-refill.hold'
   gt daemon status        # Dispatch hold: operator dispatch hold present (...
   ```
   A line reading `operator dispatch hold present` means a hold is already in place: read the file (`kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- cat seat-refill.hold`) to see whose it is. Leave another author's hold to them and start no run until it clears; a hold of your own already does this step. Any other held reason — an e-stop, the boot hold, the red hold, the Ring — stops dispatch until it clears, and the run waits. The driver's own `gt sling` is a hand sling: the hold file does not stop it.
3. **The cap is the standard dose.** `gt pool show` prints the total and each seat's cap and occupancy. Record the total and the DeepSeek seat's cap, then raise both to 8 for the run only: `gt pool set-total 8` and `gt pool set sbx-flash=8` (`sbx-flash` is the DeepSeek seat `gt pool show` lists). The driver slings onto that seat, and a sling past its cap fails.
4. **The three worktrees are at the tag.** In each of `~/gt/.runtime/rehearsal/wt-1`, `wt-2` and `wt-3`, `git status --porcelain` prints nothing and `git rev-parse HEAD` equals `git rev-parse <tag>^{commit}`. The driver runs `make presubmit` in each on its own empty Go cache.
5. **The rig's main is the tagged tree.** In `~/gt/.runtime/rehearsal/wt-1` run `git fetch origin`, then `git diff --quiet <tag> origin/main`: exit 0 means the rig's main carries the tagged commit. Any other exit means the rig was not reset to the tag; start no run until a reset to the tag makes it exit 0.
6. **The host load sampler is writing.** `tail -1 ~/gt/.runtime/host-load/$(date +%F).log` is a line from the last minute or two. The run refuses when today's file is missing.
7. **A tier sweep is running.** Nothing forces a sweep: the daemon starts a cycle on its interval. Watch `gt daemon logs -f` for `tier_sweep: cycle started` and start the run while that cycle runs. The record's `SweepRan` must read true for a standard run; the driver's verdict does not judge it.
8. **The shared sandbox cache size is read, not changed.** Read the `gt-cache` claim's used bytes: before running the node command, read step 1 of the [Sandbox rescue runbook](sandbox-rescue-runbook.md), which finds a PVC's volume name, node and path; apply it to this claim, then run `du -sb <path>` in that node (`docker --context colima-gt-sbx exec <node> du -sb <path>`). Pass the total as `--cache-bytes`; the run records it and never trims or seeds the cache.
9. **One change set is named.** Name the single change this run tests with `--change-set`. Two runs compare only under the same change set.
10. **The record name is new.** Choose the record's file name, such as `2026-11-27-a.json`; `.runtime/rehearsal/<record>.gocache` must not exist (`test ! -e`). The driver creates that directory for the three presubmits' empty Go caches and refuses a path that already exists, so never reuse a record name.

## The run

`<tag>` is the baseline tag and `<record>` is the file name chosen in Before step 10. Every path below is relative to the town root, `~/gt`:

```bash
gt-rehearsal run \
  --rig rehearsal \
  --set .runtime/rehearsal/set \
  --worktree-1 .runtime/rehearsal/wt-1 \
  --worktree-2 .runtime/rehearsal/wt-2 \
  --worktree-3 .runtime/rehearsal/wt-3 \
  --agent sbx-flash \
  --change-set "<the one change set>" \
  --tag <tag> \
  --cache-bytes <bytes> \
  --record .runtime/rehearsal/<record>
```

- `--agent` is the seat the run expects: the DeepSeek seat `gt pool show` lists.
- `--record` is the run record's JSON file; its `.gocache` cache root beside it must be new, as Before step 10 checks.
- `--intervention "<what a person did>"` records a person's step in the run and may repeat. Every intervention is a fail reason: a run a person had to step into is reported as failed. The standard setup above is not an intervention.
- The driver closes the window when all sixteen beads have closed, and at most 60 minutes in. It prints `pass`, or `fail` and one reason per broken measure: exit 0 on a pass, 1 on a fail.

A standard run is one a day, attended by the overseer. A run at 11 seats is allowed only on the operator's word: raise the total and the seat's cap to 11 for it, and keep its record apart from the standard series.

## Stop

The overseer stops a run on any of these: Dolt unanswered for two minutes (`bd list --limit 1` failing), the daemon down (`gt daemon status` fails or reports not running), or the operator's word.

1. Keep the dispatch hold on: `gt daemon status` must read `Dispatch hold: operator dispatch hold present`; if it reads clear, write it as Before step 2 does.
2. Interrupt the driver once, with Ctrl-C in its terminal. It cancels the run, closes the run's beads and saves the record marked stopped; wait for it to exit. A second signal kills the process before that save (gt-13dre6.71).
3. A stopped run is a failed run: its record is kept and its verdict is `fail`.

## Restore

Run this last on every exit once this run wrote its hold (Before step 2) or raised the caps (Before step 3): a later Before step failed, the driver refused or crashed, or the run passed, failed or was stopped. It returns the caps and the hold to the state Before recorded. After a stop for Dolt or the daemon, run it once both answer: `bd list --limit 1` succeeds and `gt daemon status` reports the daemon.

1. **Put the caps back.** If step 3 raised them: `gt pool set-total <recorded total>` and `gt pool set sbx-flash=<recorded cap>`. A total recorded as absent is `0`, which removes the total cap. `gt pool show` prints the recorded total and the seat's cap.
2. **Remove only this run's hold.** `kubectl --context colima-gt-control -n gt exec deploy/daemon -c daemon -- rm seat-refill.hold`, then `gt daemon status` reads `Dispatch hold: clear`. A hold Before step 2 found already there belongs to its author: leave it in place.
3. **A restore command that fails** leaves the hold in place and is reported with `gt escalate`; remove the hold only after the failed command succeeds.
