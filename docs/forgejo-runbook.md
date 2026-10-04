# Forgejo runbook

Read this before provisioning a rig on the local Forgejo — the bots, their
tokens and the branch protection — and before rotating a bot token. The why
behind every rule the script sets is in
[forgejo-primary-landing.md](design/forgejo-primary-landing.md); this page is
the operations.

## What `scripts/forgejo-provision.sh` converges

One run creates what is missing and leaves what already matches, so it is safe
to repeat after a partial failure:

- the bot users, one per role (`bot-polecat`, `bot-landing`, `bot-registry` by
  default; `--role viewer` adds the read-only dashboard bot the rig config also
  names), as ordinary accounts with no site-admin rights and a random password
  the script never keeps — a bot signs in by token, never by password;
- one token per bot, scoped `write:repository`, written to
  `~/.config/gt/forgejo-<role>.env` — the scope slice 4 fixes for every role,
  so the read-only `viewer` bot carries a wider scope than its use until an
  operator decides a narrower one;
- for each `--repo OWNER/NAME`, the two branch-protection rules the landing
  path depends on. `main` refuses a push from everyone, admins included, merges
  only through the landing bot, requires the gate and review contexts, and
  refuses a stale candidate. `land/*` allows a push from the landing bot and
  from deploy keys, and lets nobody else through.

Run it with `--help` for the flags.

## Before the first run

- Forgejo is up: `docker compose up -d` in `~/forgejo` (`~/forgejo/README.md`
  covers the stack, the runner and the registry).
- An admin token exists in `~/forgejo/.env` as `FORGEJO_ADMIN_TOKEN`. The
  script reads that file by default; `FORGEJO_ADMIN_TOKEN` in the environment
  wins.
- `curl` is on PATH.

## Provisioning

Bots and tokens are instance-wide; the protection rules are per repository, so
a fresh instance is two commands:

```bash
bash scripts/forgejo-provision.sh                        # bots and tokens
bash scripts/forgejo-provision.sh --repo OWNER/NAME      # one repo's rules
```

`--dry-run` reads the live state and prints what it would write without
sending a write. Expect one line per action, then `provisioning complete`; a
second run reports the users, token files and rules it found already in place.

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

Two things the script does not do:

- **Repository access.** Each bot needs write access to the rig's repository,
  or it cannot push. Add it under Repository → Settings → Collaborators with
  the Write role, the same way the web UI adds any account.
- **The push mirror and the gate workflow.** The mirror is cutover work and the
  workflow lives in the rig repo, both named in the design doc's slice list.
  The protection rules name the workflow's context, so a renamed job orphans
  the requirement: re-running the script with the new context is what reports
  the mismatch.

Verify in the web UI under Repository → Settings → Branches, and under Site
administration → User accounts.

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
| `the rule exists but Forgejo did not resolve its name` | The rule name is a branch glob (`land/*`) and Forgejo resolves it through the URL path. Set the rule by hand under Settings → Branches, then re-run. |
| `the repository is not on <api>` | The repo does not exist on this Forgejo, or the admin token cannot see it. Check `--api-url` and the repository owner. |
| A bot's push is refused | It is not a collaborator with write access, or the protection rule does not list it. Check both. |
| `token file ... is mode ...; it must be 600`, from the client | The file lost its mode. The script restores 600 on the next run. |

## Tests

`scripts/forgejo-provision_test.sh` drives every path above against a stub
Forgejo held in a temp directory — no live instance, no network — and runs
under `make test-makefile`.
