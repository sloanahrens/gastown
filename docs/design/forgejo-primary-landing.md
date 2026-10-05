# Forgejo-primary landing

Ref: gt-fn9e6.1 (epic gt-fn9e6). Companion: [mayor-retirement.md](mayor-retirement.md),
[architecture.md](architecture.md), [escalation.md](escalation.md).

The landing path moves its gate from a daemon-side function to Forgejo CI. A
polecat still pushes its branch and labels the bead; the landing worker still
builds the merge candidate, still runs om review, and is still the only writer
of `main`. What changes is where the tree is tested: a Forgejo workflow tests
the exact candidate commit, and its verdict — read from the Forgejo API —
decides the merge. GitHub stops being the origin and becomes a read-only push
mirror.

Read it before touching the landing path: the same word "gate" names the local
function in `internal/land/gate.go` today and the Forgejo job that replaces it,
and half the work is keeping the bead bookkeeping, om review and rework
semantics that no CI job can carry.

The decisions below are the epic's. This doc maps each onto the code, names the
files, and ends with the slice list the operator files beads from.

> **Superseded in part (gt-fn9e6.32).** The local gate and the
> `--force-with-lease` push this doc kept as a fallback are removed;
> `merge_queue.forgejo` is mandatory, `scripts/forgejo-rollback.sh` is gone, and
> `scripts/forgejo-resync.sh` is the one recovery. The flake policy ("No flake
> policy", Slice 2) is deleted with it: no reruns, no flake or test-budget
> beads, and a red candidate gate is a rejection. `land.LandGate`, the local
> gate function this doc names throughout, is deleted too (gt-8wbpp); the
> rig's gate is the Forgejo workflow's `make` target. Read every sentence about
> a local gate, shadow mode or a rollback as the plan it was, not as today's
> landing path.

## Method

Every path, type and config key below was read from this tree, not from memory;
`git grep -il forgejo` returns nothing, so the Forgejo client is greenfield and
every `internal/forgejo` path is new. The behaviour of the Forgejo API itself —
required status contexts, `merge_when_checks_succeed`, `head_commit_id`,
creator attribution, push mirrors — was verified against the local instance at
`http://127.0.0.1:3000` (16.0.5) in the research the epic cites, and is not
re-derived here. The doc's shape follows
[mayor-retirement.md](mayor-retirement.md): decisions, current code, ordered
slices, risks, open questions.

Constraints carried from the epic: no cloud deploys or cloud credentials in CI;
the town is flash-only; gastown keeps landing through the old path until the new
flow is proven on another rig; the mango rig is not enabled by any slice here.

## Decisions

Fixed in gt-fn9e6; change only with the operator.

- **Polecat transport.** `gt done` keeps its local presubmit, then pushes
  `polecat/<name>/<bead>+<nonce>` to Forgejo as plain transport. No CI runs on
  polecat branches.
- **Candidate gate.** The landing worker keeps its pre-merge checks and the
  throwaway-worktree merge, then pushes the merge commit as `land/<bead>`.
  A Forgejo workflow, triggered on `land/**`, one job named `gate`, context
  `ci / gate (push)`, tests that tree with the same make targets the worker
  runs today.
- **Red is rework.** The worker reworks with the tail of the failing job log,
  fetched from the Actions API. CI silence (runner down, hung) is an infra
  retry, then escalation.
- **No flake policy.** Zero tolerance: no reruns, no quarantine. The
  flake-rerun code and the per-flake beads go.
- **om review stays in the worker.** After green it runs `om review` (flash,
  never pro), posts commit status `om / review` on the candidate SHA, opens a
  PR `land/<bead> -> main`, and merges through the API with `head_commit_id`.
  Forgejo has no status webhook, so the worker polls the status API.
- **Protection.** `main` takes no push from anyone, including admins. The merge
  whitelist holds the landing bot only; the required contexts are
  `ci / gate (push)` and `om / review`; `block_on_outdated_branch` makes a
  stale PR a 409 the worker answers by rebuilding the candidate. `main` is the
  only protected branch. `land/**` carries no rule, because Forgejo refuses to
  delete any branch a rule matches — admins and the landing bot included — and
  the worker deletes `land/<bead>` after each merge (the API merge does not
  apply the repository's delete-branch-after-merge setting, which only the web
  UI's merge honours). Nothing is gated on that rule: a merge is pinned to the
  candidate's exact commit, the merge whitelist still admits only the landing
  bot, and the creator check still vets every status (gt-fn9e6.21).
- **Forgery check.** Any write user can post a commit status, so the worker
  verifies creators before merging: the gate status must have no user creator
  (it came from Actions) and `om / review` must be posted by the landing bot.
- **Bots and tokens.** One bot per role (polecat, landing, registry). Tokens
  live in `~/.config/gt/forgejo-<role>.env`, mode 600, never in
  `settings/config.json`.
- **One landing path.** Everyone lands through the worker, humans and crew
  included: push a branch, label the bead `gt:ready-to-land`. A direct UI merge
  is break-glass.
- **Mirror.** GitHub is a read-only push mirror of `main`, per-repo SSH deploy
  key, `branch_filter main`, sync-on-commit plus a 10-minute interval. A mirror
  failure never blocks a landing. A rig may instead promote its green `main`
  rather than mirror it; before cutting a rig over to promotion, read the
  cutover procedure in [the runbook](../forgejo-runbook.md).
- **Rollout.** mango, then hm, beads, om, gastown — gastown last, so the worker
  rewrite lands through the old path and is proven on other rigs first. Rigs
  that already have a gate run shadow mode (candidate pushed, Forgejo verdict
  recorded, old local gate decides) for about ten matching landings, then flip.
  mango is a watched hard cutover.

## The path today

One worker per rig, serial within the rig and parallel across rigs. No refinery
and no `gt mq` exist any more ([ADR 0004](../adr/0004-daemon-lands-work.md));
the landing worker is the only writer of `main`.

| Stage | Where | Code |
|-------|-------|------|
| Polecat pushes its branch and labels the bead | polecat worktree | `internal/done/done.go`, `markReadyToLand` |
| Pick the bead | daemon, per-rig loop | `internal/landworker/worker.go` (`Pass`, `landReady`) lists `LabelReadyToLand` |
| Pre-merge checks | daemon, bare repo | `internal/land/land.go` (`Land`), `internal/land/attribution.go`, `internal/land/riskpaths.go` |
| Merge into a throwaway worktree | `WorkRoot/wt` | `internal/land/land.go` (`addWorktree`, `mergeWork`) |
| Gate the merged tree | lander subprocess | `internal/land/gate.go` (`LandGate`, `CommandGate.Run`) |
| om review | lander subprocess | `internal/land/review.go` (`OMReviewer`) |
| Push to the target | `origin <target>` | `internal/land/land.go` (`PushForceWithLease`, `VerifyPushedCommit`) |
| Record, reap, post-land | daemon | `internal/land/land.go` (`record`), `internal/landworker/branches.go`, `internal/landworker/postland.go` |

Three facts drive everything below.

- **The gate is a function, not a service.** `LandGate` (`internal/land/gate.go`)
  merges the branch into a throwaway worktree and runs `merge_queue.gate`, else
  `make gate` split into `make gate-lint` and `make gate-test`, else `make test`
  — as a local subprocess on the operator's Mac, with the town's container
  slots. Nothing consults a CI result today.
- **Main is written by a force push, not a PR.** `Land` pushes
  `HEAD:refs/heads/<target>` with `--force-with-lease` against the base it
  merged onto. There is no landed branch and no protected-branch rule; the
  lease is the only concurrency control.
- **The candidate has no name.** The merged commit exists only as the throwaway
  worktree HEAD; `landworker/branches.go` reaps the *polecat* branch after a
  successful landing. A Forgejo gate needs the candidate to have a name CI can
  see, which is why the design introduces `land/<bead>`.

## What changes, decision by decision

One subsection per epic decision, naming the code it lands in. "New" means the
file does not exist yet.

### Candidate branch and Forgejo gate

The worker keeps `mergeWork` and the throwaway worktree, and gains a step
before the gate: push the candidate as `land/<bead>` and let Forgejo test it.
The candidate branch name is derived from `land.Work` (`internal/land/work.go`),
which already carries `BeadID`, `Branch`, `Head`, `Target`, `Worker`.

New package `internal/forgejo` holds the API client. It needs, per the epic:
commit statuses (`POST/GET /repos/{o}/{r}/statuses/{sha}`), pull requests
(create, get), merge (`POST /pulls/{n}/merge` with `head_commit_id`), job logs
(`GET /actions/runs/{id}/jobs`, `GET /actions/jobs/{id}/logs`), and the
token-per-role resolution from `~/.config/gt/forgejo-<role>.env`.

`land.LandGate` stops being the pass/fail authority and becomes the *shadow*
implementation the rollout keeps: in shadow mode the worker pushes the
candidate, records the Forgejo verdict, and the local `LandGate` still decides;
after the flip, the worker polls the required contexts instead of running
`LandGate`. The gate shape per rig (`merge_queue.gate`, the `gate-lint`/
`gate-test` split, the shell tier) becomes the Forgejo workflow's `make`
targets, which is why the workflow calls `make` rather than re-listing
commands: the local gate and the CI job must not drift.

### Rework with the failing log

`internal/landworker/worker.go` already has the rework path: `classify` maps a
red gate to `outRejectedRework`, and `reworkMessage` writes the rejection note
the polecat resumes from. The worker gains a source for the tail: today the
tail is the local gate's captured output; after the flip it is the failing
job's log fetched through the client. The rework note format does not change,
so `internal/done` and the `mol-polecat-work` formula are untouched.

CI silence — no status, or a run that never completes — is not a red gate. It
maps to `outInfra`, the existing exponential-backoff path
(`internal/landworker/worker.go`, `infraFailure`), with escalation after the
retry budget, exactly as a local lint timeout does today.

### No flake policy

This is a deletion slice, and it is larger than one file. Removing the policy
means removing:

- `internal/land/flake.go` whole: `GateBead`, `GateBeadKind` and its three
  constants, `GateBeads`, `Flake`, `NoTestNamed`, `MinPackageFlakeTests`,
  `applyFlakePolicy`, `flakeVerdict`, `fileGateBead`, `failedPackages`,
  `flakyTests`.
- From `internal/land/land.go`: the `Rerun` and `GateBeads` fields on `Lander`,
  `Result.Rerun`, `Result.Flaky`, and the whole flake branch (`Land`'s
  `!gateRes.Passed` arm) that calls `applyFlakePolicy`.
- From `internal/land/gate.go`: the `Packages`, `FailedTests` and
  `BudgetOverruns` fields on `StepResult`/`GateResult`, the `FailedTest`,
  `BudgetOverrun` and `PackageResult` types, and the parsers
  `parseGoTestOutput`, `parseTestDuration`, `parseBudgetOverruns` with their
  regexes.
- `internal/landworker/gatebeads.go` whole (`GateBeads`, `FileGateBead`,
  `LabelFlake`, `LabelTestBudget`, `FlakeTitle`, `PackageFlakeTitle`,
  `BudgetTitle`), plus the daemon wiring in `internal/daemon/landing_worker.go`
  that builds it and the `landRerun`/`rerunCommand` helpers.
- `gateRecord` must stay in some form: `Lander.record` uses it to fill
  `LandingRecord.GateResult`. Replace it with a non-flake step summary, do not
  delete the call.

A red gate becomes `RejectGate` with rework, unchanged from today's non-flaky
path. The one behavioural loss is the rerun: a test that passes on a second run
in the same tree no longer lands. The epic accepts this — flaky tests must be
fixed before a rig cuts over.

### om review and the `om / review` status

`internal/land/review.go` (`OMReviewer.Review`) already runs om and returns a
verdict; `land.go` already maps approve/request_changes and the
`ReviewErrorRejects`/`ReviewErrorLands` policy. The change is transport: after
green CI the worker posts `om / review` on the candidate SHA through the new
client, then opens and merges the PR. The overseer waiver
(`land.OverseerReviewed`, label `gt:overseer-reviewed`) becomes a success status
with the waiver recorded, so protection is satisfied and the audit trail keeps
the reason.

### The PR, the merge and the forgery check

New client calls create the PR `land/<bead> -> main` and merge it with
`head_commit_id`, so a late push voids the verdict. A 409 from
`block_on_outdated_branch` is not a rejection: the worker rebuilds the
candidate on the new main and retries, reusing the existing race path
(`outRace`).

Before merging, the worker verifies creators: the `ci / gate (push)` status
must have no user creator, and `om / review` must be the landing bot. A status
that fails either test is treated as infrastructure tampering and escalated,
not merged.

### `gt done` pushes to Forgejo

`internal/done/done.go`'s `pushBranchForLanding` pushes to `origin`, which is
hard-coded in the landing path (`internal/land/land.go` `remote()` returns
`"origin"`; the daemon's `gitRemote` wrapper names `origin`). Repointing a rig
is a remote change, not a code change: `.repo.git`'s `remote.origin.url`, the
rig registry `rig.git_url`, and every crew clone. The code change, if any, is
where a rig's remote is read instead of assumed; today there is no such
indirection.

### Crew and human landings

No change is needed. Crew already submit the way the epic requires:
`internal/done/done_crew.go` (`submitCrewForLanding`) refuses to push, requires
the branch to be on the remote, runs the local gate unless `--pre-verified`, and
then writes the same `gt:ready-to-land` marker through `markReadyToLand`. The
worker cannot tell a crew submission from a polecat one, so "everyone lands
through the worker" already holds.

### Config

The per-rig settings live in the repo-committed `.gastown/settings.json`, the
rig root `config.json`, and the rig-local `settings/config.json`, resolved by
`rig.ResolveMergeQueueConfig` (`internal/rig/manager.go`) into
`config.MergeQueueConfig` (`internal/config/types.go`). The daemon reads
`merge_queue.gate`, `merge_queue.post_land_command` and the Makefile fallbacks.

New per-rig keys: the Forgejo remote URL, the gate workflow name (so the
required context is derived, not typed twice), the bot logins, and the mirror
target. They sit beside `merge_queue` in the same three files and resolve
through the same precedence. The token *files* are host facts, not rig config:
`~/.config/gt/forgejo-<role>.env` is named by role and read by the client, so no
secret lands in a repo-committed file or in `settings/config.json`.

### Mirror and cutover tooling

The mirror is Forgejo-side (push mirror, `branch_filter main`, sync-on-commit,
10-minute interval) plus a town monitor that reads `last_error` and
`last_update` from `GET /repos/{o}/{r}/push_mirrors` and alerts past 30
minutes. Cutover and rollback are scripts under `scripts/`, driven by the
rollout order in the epic; they repoint origins and are not part of the worker.

## Risks

- **Merged-tree semantics.** The worker tests the tree it is about to land; a
  Forgejo PR check runs on the merge ref (base merged into head), which for a
  `land/<bead>` branch cut from current `main` is the candidate tree only if
  `main` has not moved. `block_on_outdated_branch` plus the 409 rebuild is the
  guard, but every rebuild is a fresh CI run, so a busy `main` multiplies gate
  runs. The worker must treat the rebuild as the normal case, not an error.
- **Required-context coupling.** A required context that never reports blocks
  forever, and renaming the workflow or job silently orphans the rule. The
  context string must be derived from one place (the config key naming the
  workflow) and a startup check must confirm each required context has reported
  at least once.
- **Forgery.** Any write-access user can post a commit status. The creator
  checks are the only defence and they are load-bearing; a bot with `admin`
  collaborator rights or the admin token would defeat the whole gate.
- **Flake removal is a behaviour change on the live path.** Dropping the rerun
  makes today's local gate stricter before any rig has CI. Either land it early
  and accept stricter landings town-wide, or land it per rig at flip time.
- **Container slots.** hm's gate needs Docker and holds one of the town's eight
  slots today. A Forgejo runner on the same host must use the same pool or the
  arbitration breaks; the epic puts CI on its own capacity-3 pool, which is a
  second pool on one host.
- **Escalation and jam.** With CI remote, a Forgejo outage stops all landing on
  every cut-over rig at once. The infra-retry path covers the worker; the
  polecat's `gt done` push failure must fail closed so work is not lost.
- **`gt done`'s local presubmit stays.** It runs `make presubmit`, not the full
  `make gate`, so a polecat branch is untested by the full tier until CI. That
  is intended (transport only) but means CI is the first full gate, and CI
  capacity becomes the landing rate limit.

## What this bead does not do

No code changes. No slice beads are filed — the operator files them from the
list at the end of this doc. No rig is onboarded; in particular the mango rig is
not enabled here. The Forgejo stack at `~/forgejo` is not changed.

## Where the epic cannot be followed exactly

- **The container slot does not move into CI.** hm's gate is `make test` under
  `gt slot run`, holding one of the town's eight container slots. Forgejo runs
  its own capacity-3 pool, so the workflow calls the underlying make target
  without the slot wrapper and the town's arbitration no longer covers that
  test. The epic accepts a separate CI pool; the consequence is that hm's
  landing gate and its CI no longer share a limiter.
- **"Flash only, never pro" is not enforced in the landing code.** Nothing in
  `internal/land` names a model: om reads its backend from operator config
  (`~/.config/om/config.json`, flash today) and ignores a `backend` key in the
  reviewed tree's `.om.json`, so that file carries only the rubric and review
  knobs. Forcing the backend from the review command is a separate build item
  this doc does not take.
- **The required context is the push context, not the PR context.** The gate
  job fires on `land/**` and reports `ci / gate (push)`. A PR opened from
  `land/<bead>` also gets a `ci / gate (pull_request)` check on the merge ref,
  which is a different context and a different tree. Protection must require
  the push context; requiring the PR one would test the wrong tree and could
  fail on `main`'s content.
- **The forgery check's "no user creator" is a Forgejo behaviour, not a
  contract.** It was verified on 16.0.5. The creator check should fail closed
  and be re-verified on upgrade; a Forgejo version that starts attributing
  Actions statuses to a user would block every merge.

## Open questions for the operator

Each has a recommended answer; the recommendation is the default unless the
operator says otherwise.

1. **Who owns the required-context list?** The workflow file and the branch
   protection must agree on `ci / gate (push)`, and a rename silently orphans
   the rule. Recommend: the workflow file owns the job name, per-rig config
   names the workflow, and the worker's startup asserts each required context
   has reported at least once on a known commit.
2. **Mirror credential.** The epic leaves it undecided. Recommend the
   Forgejo-generated Ed25519 deploy key on the GitHub repo, because it is
   per-repo, revocable, and needs no token rotation.
3. **Flake removal timing.** Recommend landing slice 2 early and town-wide: it
   is the epic's policy, it removes code before the rewrite, and the stricter
   live path is the accepted trade.
4. **Tags on the mirror.** `branch_filter main` mirrors branches but not tags.
   Recommend deciding now: if releases are cut from GitHub, mirror tags too
   (a second filter entry or a second mirror); otherwise accept no tags.
5. **Forgejo down.** The epic lists this as not yet specified. Recommend
   `gt done` fails closed (the push error keeps the polecat session up) and the
   worker treats it as an infra retry, so no work is lost and no landing
   silently stalls.
6. **Keep or delete `land/<bead>` after merge?** Recommend
   `delete_branch_after_merge`, since the polecat branch is already reaped and
   the landed SHA lives in the landings record.
7. **Runner capacity.** The epic sets cap 3. Recommend starting there,
   measuring during shadow mode, and raising before gastown — the last rig —
   flips, or CI becomes the landing rate limit.
8. **GitHub Actions at cutover.** The epic says remove them to stop cloud
   minutes, but asks first. Recommend disabling (not deleting) each rig's
   `.github/workflows/ci.yml` at cutover, so rollback can re-enable it.

## Slices

Twelve slices. **Every slice is one worker and one MR**, and every slice's gate
is `make gate`. Each names its files, its `Needs` (dependencies — "needs" means
"cannot start until", not "comes before") and its `Covers` (the epic item it
discharges). A new test package that reaches beads or the town runs
`testutil.HermeticMain` in its `TestMain` (internal/testpolicy enforces this at
the gate).

Landing order is not slice order: 1, 3 and 4 can land before any rig is
onboarded; 2 changes the live path (see open question 3); 5–8 rewrite the
worker; 9–12 are cutover. The rollout order (mango, hm, beads, om, gastown) is
in the epic and is not repeated here.

### Slice 1 — Forgejo API client

New package `internal/forgejo`. No live Forgejo in unit tests: drive it against
`httptest`. It covers the calls the worker and the monitor need, and nothing
else.

Files: `internal/forgejo/client.go`, `internal/forgejo/status.go`
(`GET/POST /repos/{o}/{r}/statuses/{sha}`, combined status), `internal/forgejo/pulls.go`
(create, get, merge with `head_commit_id`), `internal/forgejo/actions.go`
(`/actions/runs`, `/runs/{id}/jobs`, `/jobs/{id}/logs`), `internal/forgejo/mirror.go`
(`GET/POST /repos/{o}/{r}/push_mirrors`), `internal/forgejo/token.go` (resolve
`~/.config/gt/forgejo-<role>.env`, mode 600; the value never lands in config),
`internal/forgejo/client_test.go`, `internal/forgejo/testdata/`.

Needs: none. Covers: "a Forgejo client package (statuses, PRs, merge, job
logs, bots/tokens)".

### Slice 2 — Remove the flake policy

The deletion in "No flake policy" above, and nothing else. It changes the live
path, so it lands on its own and is called out in the landing note.

Files: `internal/land/flake.go` (delete), `internal/land/flake_test.go`
(delete), `internal/land/land.go`, `internal/land/gate.go`,
`internal/landworker/gatebeads.go` (delete), `internal/landworker/gatebeads_test.go`
(delete), `internal/daemon/landing_worker.go`, and every test that names
`Rerun`, `GateBeads` or `applyFlakePolicy`.

Needs: none. Covers: "removal of flake policy".

### Slice 3 — Per-rig Forgejo settings

The config keys, and the readers that resolve them. Tokens are not config: the
key names the role, and the client reads the env file.

Files: `internal/config/types.go` (a `ForgejoConfig` block on
`MergeQueueConfig` or `RigSettings`: remote URL, gate workflow name, bot
logins, mirror target), `internal/config/daemon_config.go`
(`LandingWorkerConfig`), `internal/rig/manager.go` (a resolver beside
`ResolveMergeQueueConfig`).

Needs: none. Covers: "per-rig config (remote and gate workflow)".

### Slice 4 — Forgejo provisioning tooling

Create the bot users, mint tokens (scopes: `write:repository`), set `main`
protection (no push for anyone, merge whitelist = landing bot, required
contexts, `block_on_outdated_branch`), and leave `land/**` unprotected so the
worker can delete the candidate branch after each merge. Idempotent script plus
a runbook.

Files: `scripts/forgejo-provision.sh` (new), `docs/forgejo-runbook.md` (new;
a `> Status:` header is not used — this is a live runbook, not a historical
doc).

Needs: 1. Covers: bots/tokens and the branch-protection half of "per-rig
config".

### Slice 5 — Candidate branch and the CI verdict

The worker pushes the merge candidate as `land/<bead>`, then waits for the
`ci / gate (push)` context on that SHA. Red is a rework carrying the failing
job's log tail; silence is the existing infra backoff. Either outcome deletes
the branch the run pushed, best-effort, so a bead refused for good leaves none
behind (gt-k796q). `LandGate` is still built and still available, because
shadow mode (slice 8) uses it.

Files: `internal/land/candidate.go` (new: branch naming, push, poll),
`internal/land/land.go` (`Land` calls the candidate step before the gate),
`internal/land/work.go` (`Work` gains the candidate fields),
`internal/landworker/worker.go` (`classify` distinguishes CI red from CI
silence), `internal/daemon/landing_worker.go` (wiring, the CI client, remote),
`internal/land/candidate_test.go`.

Needs: 1, 3. Covers: "worker candidate flow".

### Slice 6 — om / review status, PR and merge

After green: run om, post `om / review` on the candidate SHA, open
`land/<bead> -> main`, merge with `head_commit_id`. A 409 from
`block_on_outdated_branch` rebuilds the candidate and retries through the
existing race path.

Files: `internal/land/review.go`, `internal/land/land.go`,
`internal/landworker/worker.go`, `internal/daemon/landing_worker.go`.

Needs: 5. Covers: "om / review status posting".

### Slice 7 — Creator verification

Before merging, assert the gate status has no user creator and `om / review`
was posted by the landing bot. A status failing either check is escalated, never
merged.

Files: `internal/forgejo/status.go`, `internal/land/land.go`,
`internal/land/land_test.go`.

Needs: 6. Covers: "creator checks". Small; fold into slice 6 if the operator
prefers one MR.

### Slice 8 — Shadow mode

A per-rig flag: the worker pushes the candidate and records the Forgejo verdict,
and the local `LandGate` still decides. Both verdicts are recorded so the
flip/no-flip call has evidence.

Files: `internal/config/types.go`, `internal/land/land.go`,
`internal/landworker/worker.go`, `internal/land/note.go` (`LandingRecord` gains
the CI fields), `internal/landings/landings.go` (the reader's `Record` mirrors
the writer — a test pins the keys), `internal/daemon/landing_worker.go`.

Needs: 5, 6. Covers: "shadow mode".

### Slice 9 — The configured remote in `gt done` and the lander

Today `origin` is hard-coded: `Lander.remote()` returns `"origin"`, the
daemon's `gitRemote` names it, and `git.RemoteForRef` accepts only `origin` and
`upstream`. Repointing a rig is a remote change, so the code change is only
where a remote name is assumed and should be read from slice 3's settings.

Files: `internal/git/git.go` (`RemoteForRef`), `internal/land/land.go`,
`internal/done/done.go` (`pushBranchToOrigin`, `verifyPushLanded`),
`internal/daemon/landing_worker.go`, `internal/rig/manager.go`.

Needs: 3. Covers: "done flow pushing to the Forgejo remote". If the operator
repoints `origin` instead, this slice is a no-op and should be closed with that
reason.

### Slice 10 — Gate workflow per rig and the runner label image

One workflow file per rig repo, `.forgejo/workflows/gate.yml`, one job `gate`,
running the rig's `make` targets — the same targets `merge_queue.gate` names.
The workflow is the single source of the job name, so slice 3's config derives
the required context from it rather than typing the string twice. The runner
label image already exists at `~/forgejo/runner-image/Dockerfile`; this slice
extends it if a rig needs more than Go, Node and the docker CLI.

Files: `.forgejo/workflows/gate.yml` (new, this repo), the same file in each
other rig's repo (named here, landed there), and the runner image (host file,
outside this repo).

Needs: none for the file; the flip needs 5. Covers: the gate-workflow half of
"per-rig config".

### Slice 11 — Mirror monitor

The push mirror itself is Forgejo-side; the monitor is ours. Poll
`GET /repos/{o}/{r}/push_mirrors`, alert on `last_error`, and on
`last_update` older than 30 minutes. A mirror failure never blocks a landing.

Files: a daemon patrol beside the landing worker in `internal/daemon/`,
`internal/townhealth/` for the status line, `internal/config/daemon_config.go`.

Needs: 1, 3. Covers: "mirror monitor".

### Slice 12 — Cutover and rollback tooling

Pause landing, preflight `GitHub main == Forgejo main`, repoint `.repo.git`
origins, `rig.git_url` and crew clones, enable the mirror, resume. Rollback is
the reverse, triggered by two consecutive infra failures.

Files: `scripts/forgejo-cutover.sh` (new), `scripts/forgejo-rollback.sh` (new),
`docs/forgejo-runbook.md` (the runbook from slice 4 grows the cutover section).

Needs: 9, 10, 11. Covers: "cutover tooling".

