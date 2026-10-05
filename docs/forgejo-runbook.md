# Forgejo runbook

Read this before provisioning a rig on the local Forgejo — the bots, their
tokens, their repository access and the branch protection — and before rotating
a bot token. The why behind every rule the script sets is in
[forgejo-primary-landing.md](design/forgejo-primary-landing.md); this page is
the operations.

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
- for each `--repo OWNER/NAME`, each role's collaborator access and then the one
  branch-protection rule the landing path depends on. The access comes first on
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
package registry alone. The grant is read before it is written and lands before
the protection rule is created or patched, so a second run on unchanged state
sends no write.

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

The gate workflow lives in the rig repo, and the push mirror is the cutover's
work (below). The protection rule names the workflow's context, so a renamed
job orphans the requirement: re-running the provisioner with the new context is
what reports the mismatch.

Verify in the web UI under Repository → Settings → Branches, and under Site
administration → User accounts.

## Cutting a rig over

Before a rig's cutover its gate workflow is on the GitHub `main` — landed
through the old path first, so Forgejo never falls back to
`.github/workflows` — and its runner image can run that gate.

`bash scripts/forgejo-cutover.sh <rig> --repo OWNER/NAME` runs the procedure;
`--help` lists the flags, and the script header gives the order and the reason
for each step. It refuses while a `gt:ready-to-land` bead is queued and when
the probe it runs is not green, so a cutover that starts is one whose gate the
real runner has already passed. `--dry-run` prints every command, the probe
included, and writes nothing.

The mirror carries no branch filter on purpose: Forgejo then pushes every ref
and prunes every GitHub ref it does not hold, which is why the import step runs
first and why skipping it loses GitHub-only branches.

Four things stay with the operator, and the script prints them at the end:

- the `gh repo deploy-key add` command for the mirror's deploy key. Forgejo
  mints a new keypair per mirror and offers no API to replace one, so the key
  goes on GitHub by hand; until it is there the mirror syncs nothing;
- GitHub Actions disabled on the GitHub repo;
- any self-hosted GitHub runner for it stopped, not removed;
- the gate workflow unchanged in the Forgejo copy.

Then watch the first landing: `ci / gate (push)` green with no user creator,
the PR merged by `bot-landing` as a fast-forward whose commit is the candidate,
`om / review` created by the same bot, and Forgejo `main` equal to GitHub
`main`.

Every file the script edits is copied to a dated `.bak-` sibling first, and a
second run over converged state reports each step as already converged and
writes nothing.

## Rolling a rig back

Rollback is an operator move, never an automatic one. Run it when Forgejo or
the runner has been down 30 minutes with landings queued, after two
consecutive infrastructure failures, or when a mirror error will not clear.

`bash scripts/forgejo-rollback.sh <rig>` stops the mirror, removes the rig's
Forgejo block and repoints the rig at GitHub; `--help` lists the flags. The
order is load-bearing: the mirror stops and the `gh repo deploy-key delete`
command is printed before anything is repointed, because a mirror still running
pushes every ref to GitHub and would overwrite whatever lands there.

The daemon restart is the one step left to the operator when a landing is
queued — repointing mid-landing is safe, restarting over one is not. The script
prints the command for that case. A landing through the old path is what proves
the rollback, as the mango rehearsal did.

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
`scripts/forgejo-cutover_test.sh` and `scripts/forgejo-rollback_test.sh` drive
every path above against a stub Forgejo and a stub town held in a temp
directory — no live instance, no network — and run under `make test-makefile`.
