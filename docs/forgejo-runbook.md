# Forgejo runbook

Read this before provisioning a rig on the local Forgejo, cutting one over or
rolling one back, resyncing Forgejo from GitHub, rotating a bot token, or
acting on a landing failure. It covers the bots, their tokens, their repository
access and the branch protection, the cutover, rollback and break-glass
procedures, and every class of landing failure. The why behind every rule the
script sets is in [forgejo-primary-landing.md](design/forgejo-primary-landing.md);
this page is the operations.

## What `scripts/forgejo-provision.sh` converges

One run creates what is missing and leaves what already matches, so it is safe
to repeat after a partial failure:

- the bot users, one per role (`bot-polecat`, `bot-landing`, `bot-registry` by
  default; `--role viewer` adds the read-only dashboard bot the rig config also
  names), as ordinary accounts with no site-admin rights and a random password
  the script never keeps — a bot signs in by token, never by password;
- one token per bot, written to `~/.config/gt/forgejo-<role>.env`, scoped to
  what that role does: `polecat` `write:repository`; `landing`
  `write:repository,read:user` (it reads its own login); `registry`
  `write:package,read:package` only, which pushes images with no repository
  scope at all; `viewer` `read:repository,read:user`. An unknown role is an
  error, not a default scope. A token minted by an earlier version keeps its
  old scope until `--rotate` replaces it;
- for each `--repo OWNER/NAME`, each role's collaborator access, the viewer
  bot's read access, and then the one branch-protection rule the landing path
  depends on. The access comes first on
  purpose: Forgejo drops a `merge_whitelist_usernames` entry for a login that is
  not yet a collaborator with write access, so writing the rule first leaves a
  fresh repo with an empty merge whitelist and nobody, admins included, able to
  merge. `main` refuses a push
  from everyone, admins included, merges only through the landing bot, requires
  the gate and review contexts, and refuses a stale candidate. `main` is the
  only protected branch: Forgejo refuses to delete a branch any protection rule
  matches, admins and the landing bot included, and the worker deletes
  `land/<bead>` after every merge (the API merge does not apply the
  repository's delete-branch-after-merge setting, which only the web UI's merge
  honours). A run reports a `land/*` rule left by an earlier cutover and
  removes it; under `--dry-run` it reports the removal without sending it.
  A protection write whose read-back differs from what was sent exits
  non-zero, so a field Forgejo dropped stops the run instead of leaving a rule
  nobody can merge through.

Run it with `--help` for the flags.

## Before the first run

- Forgejo is up: `docker compose up -d` in `~/forgejo` (`~/forgejo/README.md`
  covers the stack, the runner and the registry).
- An admin token exists in `~/forgejo/.env` as `FORGEJO_ADMIN_TOKEN`. The
  script reads that file by default; `FORGEJO_ADMIN_TOKEN` in the environment
  wins.
- `curl` is on PATH.

## Provisioning

Bots and tokens are instance-wide; collaborator access and the protection rules
are per repository, so a fresh instance is two commands:

```bash
bash scripts/forgejo-provision.sh                        # bots and tokens
bash scripts/forgejo-provision.sh --repo OWNER/NAME      # one repo's access and rules
```

With `--repo`, each role's bot is granted the access it needs — `write` for
polecat and landing, `read` for viewer, none for registry, which works in the
package registry alone — and the viewer bot is granted its read even when the
run does not name the viewer role, so every repo a run provisions is one the
dashboard's pane can read. The grant is read before it is written and lands
before the protection rule is created or patched, so a second run on unchanged
state sends no write.

The viewer bot is not in the default role list, so on an instance that has no
`bot-viewer` yet a repo run reports a notice instead of the grant, and the pane
names that repo as one it cannot read rather than showing it as a rig with no
landings. One run creates the bot and its token and grants the repo:

```bash
bash scripts/forgejo-provision.sh --role viewer --repo OWNER/NAME
```

`--dry-run` reads the live state and prints what it would write without
sending a write. Expect one line per action, then `provisioning complete`; a
second run reports the users, token files, access and rules it found already in
place.

The bot logins and gate workflow a rig uses are its operator-only
`merge_queue.forgejo` settings; the flags here must carry the same values, and
a renamed workflow means re-running with the new context.

No stream ever shows a token value. Each minted token is written only to its
role's env file, mode 600, in this shape:

```
FORGEJO_URL=http://127.0.0.1:3000
FORGEJO_USER=bot-landing
FORGEJO_TOKEN=<the value>
```

`internal/forgejo` reads the `FORGEJO_TOKEN` line and refuses a file any group
or other user can read. A token never belongs in a rig's committed settings or
in `settings/config.json`: the config names the role, the client reads the
file.

## After the bots exist

The gate workflow lives in the rig repo, and connecting the rig to GitHub — a
promotion by default, a push mirror with `--mirror` — is the cutover's work
(below). The protection rule names the workflow's context, so a renamed job
orphans the requirement: re-running the provisioner with the new context is
what reports the mismatch.

Verify in the web UI under Repository → Settings → Branches, and under Site
administration → User accounts.

## Per-rig facts

Each rig's cutover needs its GitHub repo, its gate command and the facts that
change the procedure:

| Rig | GitHub repo | Gate command | Facts that matter |
|-----|-------------|--------------|-------------------|
| mango | `mango` | `make gate` | cut over; no deploy possible |
| beads | `beads`, a public fork of `gastownhall/beads` | `make gate` | cut over and parked on demand; Actions disabled; 18 upstream workflows |
| om | `organic-mechanic` | `make gate` | Go 1.27; a self-hosted GitHub runner as a launchd service — stop it, keep it installed; 5 branches |
| hm | `history-man` | `make lint test` (no `gate` target) | Go 1.27; shellcheck in lint; its landing gate starts containers, so the probe must confirm Docker access works for the unprivileged `ci` user or that those tests skip; a self-hosted GitHub runner; 3 branches |
| gastown | `gastown` (public) | `make lint-tools`, then `make gate` | the gate workflow is already on `main`; about 170 branches to import; its `ci.yml` runs on a GitHub-hosted runner — disable it; it lands through Forgejo like the other rigs; its break-glass is the resync procedure below, and has not been rehearsed on gastown, which hosts the worker, so `bash scripts/forgejo-rollback.sh gastown` is the first move if the Forgejo path breaks |

Every rig's precondition is the same: its gate workflow is on GitHub `main`
(landed through the old path first, so Forgejo never falls back to
`.github/workflows`), the probe is green, every GitHub ref is imported, the
operator has approved the cutover's deploy key (the promote key by default, the
mirror's with `--mirror`), and the operator has decided what to do with GitHub
Actions. The runner image in `~/forgejo/runner-image` is
part of it: Go 1.27.1, Node 20, jq, shellcheck, lsof, procps, dolt, the Docker
CLI, an unprivileged `ci` user, job containers started with `--init`, and Go
and npm cache volumes.

## When a landing fails

Every landing outcome belongs to one class, and the class decides who acts:

| Class | Meaning | The system | The operator |
|-------|---------|------------|--------------|
| Work | the change caused a red | reworks the bead to the polecat with the failure excerpt | nothing |
| Infrastructure | the runner, Forgejo, Docker or the network failed | retries with backoff, and escalates to a human after three in a row | fix the machinery; watch the alert |
| Policy | `om` rejected, a forged status, a refused merge | escalates to a human with the `gt:needs-human` label | decide |
| Operator | cutover, config, mirror or image | nothing automatic | run the procedure on this page |

The failures seen on the live rigs so far, each closed or ticketed:

| Failure | Class | The system | The operator |
|---------|-------|------------|--------------|
| CI red on a test the change touches | Work | reworks with the failure excerpt (gt-fn9e6.25) | nothing |
| CI red on an unrelated test because the runner image is wrong (a missing tool, Go skew) | Operator | reads it as a work red — a user step ran, so the log carries no infrastructure signature | the polecat escalates; fix the image, then `gt land requeue <bead> --reason "<why>"` |
| A job container had no init, so zombies broke a process-kill test | Operator | — | start job containers with `--init` |
| A run was cancelled | Infrastructure | reads the run status as infrastructure, not a red (gt-fn9e6.16), and retries | watch the alert |
| A job failed before any user step ran (image pull, container start) | Infrastructure | matches the start-failure signature — no `Run Main` marker in the job log (gt-fn9e6.30) — and retries | fix the machinery; watch the alert |
| The runner is offline or lost, so no status arrives within the CI wait | Infrastructure | reads the missing status as CI silence and retries | bring the runner back |
| Forgejo is unreachable | Infrastructure | the worker backs off; `gt done` fails closed and the polecat's session stays up | bring Forgejo back |
| The landing deadline is exceeded | Infrastructure | reads the deadline as infrastructure, not a rejection | fix the machinery |
| Three consecutive infrastructure failures | Policy | escalates to a human | decide |
| `om / review` rejects | Policy | reworks to the polecat, or escalates when the rejection is not a rework | decide |
| A forged status | Policy | refuses with a `forged_status` rejection and escalates | decide |
| A merge is refused (405) | Policy | refuses with a `merge_refused` rejection and escalates | decide |
| The branch is outdated (409) | — | rebuilds the candidate and retries | nothing |
| A rejected head cannot be resubmitted (`gt done` ends `DONE_CLOSE_SKIPPED`) | Operator | `gt done` stands down and says why (gt-3e1z4) | `gt land requeue <bead> --reason "<why>"` |
| A push to Forgejo has no git credentials | Operator | — | install the role-keyed credential helper (`~/forgejo/README.md`) |
| The mirror prunes a GitHub-only ref | Operator | — | import every GitHub ref before the mirror exists (see "Cutting a rig over") |
| A protected branch cannot be deleted | Operator | — | only `main` is protected (gt-fn9e6.21) |
| The dashboard's Forgejo pane names a rig repo it cannot read | Operator | reads that repo's landings not at all, and says so in the panel instead of looking quiet (gt-faml5) | `bash scripts/forgejo-provision.sh --role viewer --repo OWNER/NAME` |
| Provisioning a fresh repo never converges | Operator | — | the collaborator grant lands before the rule (gt-fn9e6.23) |
| The startup context check alarms after a restart | Operator | raises `landing-forgejo-context:<rig>` | `gt escalate clear --fingerprint landing-forgejo-context:<rig>` once the landing is known good (gt-fn9e6.24) |
| A rig's new Forgejo block is read only at daemon start | Operator | — | restart the daemon when no landing is in flight (see "Cutting a rig over") |
| GitHub `main` is not an ancestor of the green commit that wanted promoting | Operator | leaves GitHub alone and raises `landing-promote-diverged:<rig>` | reconcile GitHub `main` with Forgejo's, then `gt escalate clear --fingerprint landing-promote-diverged:<rig>` (gt-fn9e6.37) |

## Cutting a rig over

Before a rig's cutover its gate workflow is on the GitHub `main` — landed
through the old path first, so Forgejo never falls back to
`.github/workflows` — and its runner image can run that gate.

`bash scripts/forgejo-probe.sh <rig> --repo OWNER/NAME` proves the last of
those before anything changes: it clones the rig's GitHub `main`, pushes it to
the rig's Forgejo copy as `land/probe-<rig>` from a throwaway worktree of the
rig's own repository — the same push the landing worker makes, so the rig's
pre-push hook runs and a hook that refuses `land/*` fails the probe rather than
the first landing — then waits for the real gate status and requires green
before deleting the branch and the worktree. `--town-root` says which town holds
the rig; where that repository is absent the probe pushes from the clone
instead and warns that the hooks were not exercised. The cutover runs the probe
and refuses unless it is green, so a cutover that starts is one the real runner
has already passed. A probe leaves no record, so `--dry-run` prints the probe
instead of running it.

`bash scripts/forgejo-cutover.sh <rig> --repo OWNER/NAME` runs the procedure;
`--help` lists the flags, and the script header gives the order and the reason
for each step. It refuses while a `gt:ready-to-land` bead is queued.
`--dry-run` prints every command and writes nothing.

When the rig's credential-helper hostname is not the admin base — the usual
case, since it is the form every rig remote carries — pass `--forgejo-url` that
hostname form. It is what lands in the remotes, `town.json` and the rig block;
the script's own git work rides the admin base instead, because it runs before
the bots have access, so leaving the flag off writes an admin URL the bots
cannot use.

By default the cutover promotes: it mints an ed25519 keypair under the config
dir (`promote-<rig>.key`, mode 600, reused on a second run and never printed)
and writes `promote_target` and `promote_key_file` into the rig block, so a
green main verdict fast-forwards GitHub `main` through `internal/promote`
instead of a mirror that pushes every commit before the slow tiers. With
`--mirror` it creates the push mirror instead, which carries no branch filter on
purpose: Forgejo then pushes every ref and prunes every GitHub ref it does not
hold, which is why the import step runs first and why skipping it loses
GitHub-only branches. It syncs on commit and every 10 minutes, and GitHub keeps
a ref Forgejo has deleted until that next sync.

A rig that promotes has no push mirror for `main`: GitHub's `main` is advanced
only by the promotion in `internal/promote`, which pushes the green commit as
`<commit>:refs/heads/main` and only when that is a fast-forward. Promotion rides
the two checks that call a commit green — the green post-land verdict at the
landed commit (`internal/landworker`) and a scheduled sweep that covered every
tier green (`internal/daemon`) — so GitHub's `main` is the last commit both a
post-land run and the sweep called good, never a commit pushed ahead of any
check. A target whose `main` is not an ancestor of the green commit is left
alone and raises `landing-promote-diverged:<rig>`; before reconciling the two,
read the divergence row in "When a landing fails".

Four things stay with the operator, and the script prints them at the end:

- the `gh repo deploy-key add` command for the cutover's deploy key — the
  promote key's public half by default, the mirror's key with `--mirror`. The
  script mints the promote keypair itself; Forgejo mints a new keypair per
  mirror and offers no API to replace one. Either key goes on GitHub by hand,
  and until it is there a promotion cannot push and the mirror syncs nothing;
- GitHub Actions disabled on the GitHub repo — the script prints the exact
  command, `gh api --method PUT repos/OWNER/NAME/actions/permissions -F
  enabled=false`. It must be `-F` (the value is typed) and not `-f` (which sends
  a string), or GitHub answers 422;
- any self-hosted GitHub runner for it stopped, not removed;
- the gate workflow unchanged in the Forgejo copy.

Then prove the connection: with the default, the rig's next green main verdict
advances GitHub `main`; with `--mirror`, the mirror's first sync and the rig's
next landing, after which the rig's GitHub-only workflows, if any remain, can
go.

The daemon reads a rig's Forgejo block only at start, so a cutover restarts it
(`gt daemon restart`) — but only when no landing is in flight. Restarting over
a queued landing drops it; repointing a remote mid-landing is safe.

Then watch the first landing: `ci / gate (push)` green with no user creator,
the PR merged by `bot-landing` as a fast-forward whose commit is the candidate,
`om / review` created by the same bot, and Forgejo `main` equal to GitHub
`main`.

Every file the script edits is copied to a dated `.bak-` sibling first — a
`--dry-run` prints the copy it would make and leaves none — and a second run
over converged state reports each step as already converged and writes nothing.

## Rolling a rig back

Rollback is an operator move, never an automatic one. Run it when Forgejo or
the runner has been down 30 minutes with landings queued, after two
consecutive infrastructure failures, or when a mirror error will not clear.

`bash scripts/forgejo-rollback.sh <rig>` removes the rig's Forgejo block
(carrying any `promote_target` and `promote_key_file` with it), deletes the
promote keypair the cutover minted, stops a push mirror when the block names
one, and repoints the rig at GitHub; `--help` lists the flags. The order is
load-bearing: a running mirror stops and the `gh repo deploy-key delete` command
is printed before anything is repointed, because a mirror still running pushes
every ref to GitHub and would overwrite whatever lands there.

A rollback runs while landings are queued, so it obeys the daemon-restart rule
above by printing the command instead of forcing it. A landing through the old
path is what proves the rollback, as the mango rehearsal did.

## Resyncing and break-glass

`bash scripts/forgejo-resync.sh OWNER/NAME` makes a Forgejo repository's refs
match GitHub's and restores `main` protection. Two uses:

- **GitHub moved while a rig was rolled back.** A rolled-back rig lands on
  GitHub, so Forgejo can fall behind it.
- **Break-glass.** When the Forgejo landing path itself is broken, the fix
  cannot land through it. Push the fix to the rig's GitHub repository, then
  resync, and the fix becomes Forgejo's `main`.

`main` refuses every push, admins included, so nothing is pushed to Forgejo
directly. The script lifts the `main` rule (backing it up first), pushes every
GitHub ref to Forgejo with `--prune`, and restores the rule by re-running
`forgejo-provision.sh --repo OWNER/NAME`. The restore runs from an exit trap,
so a push that fails still leaves `main` protected. `--dry-run` prints every
step and writes nothing.

gastown lands through Forgejo like every other rig, and its break-glass is the
resync procedure above rather than a separate one. It has not been rehearsed on
gastown itself, which hosts the worker, so `bash scripts/forgejo-rollback.sh
gastown` stays the first move when the Forgejo path breaks.

## Rotating a token

A token's value is returned once, at creation. A lost or leaked token is
replaced, never recovered:

```bash
bash scripts/forgejo-provision.sh --rotate
```

Rotation revokes the previous token of the same name and mints a replacement
into the same file. Nothing else needs the value copied anywhere — every
consumer reads the file — but a process that read it before the rotation holds
the revoked value until it restarts.

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| `the admin token cannot mint a token for <bot>` | The token lacks site-admin rights. `POST /users/{name}/tokens` wants HttpBasic auth by design, so only a site-admin token on the `/admin/users/{name}/tokens` route can mint for another account. |
| `the rule exists but Forgejo did not resolve its name` | Forgejo resolves a rule name through the URL path, so a name holding `/` is sent encoded. Set the rule by hand under Settings → Branches, then re-run. |
| `the repository is not on <api>` | The repo does not exist on this Forgejo, or the admin token cannot see it. Check `--api-url` and the repository owner. |
| `the write for main on <repo> did not take` | Forgejo accepted the request but its response omitted a field the run sent — commonly a `merge_whitelist_usernames` entry for a login that is not a collaborator with write access. The run exits non-zero and leaves the rule as Forgejo stored it. Grant the named login access (a normal `--repo` run does this for the roles it provisions) and re-run. |
| A bot's push is refused | It is not a collaborator with write access, or the protection rule does not list it. Check both. |
| `token file ... is mode ...; it must be 600`, from the client | The file lost its mode. The script restores 600 on the next run. |

## Tests

`scripts/forgejo-provision_test.sh`, `scripts/forgejo-probe_test.sh`,
`scripts/forgejo-cutover_test.sh`, `scripts/forgejo-rollback_test.sh` and
`scripts/forgejo-resync_test.sh` drive every path above against a stub Forgejo
and a stub town held in a temp directory — no live instance, no network — and
run under `make test-makefile`.
