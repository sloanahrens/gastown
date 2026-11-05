+++
name = "app-promote"
description = "Promote each app rig's newest prod-proven release commit to its GitHub target (GitHub follows prod)"
version = 1

[gate]
type = "cooldown"
duration = "10m"

[tracking]
labels = ["plugin:app-promote", "category:release"]
digest = true

[execution]
type = "script"
binary = "bin/app-promote"
timeout = "5m"
+++

# App Promote

GitHub follows prod (dv-jokk, gt-vdhwk): this plugin advances each app rig's
GitHub `main` to the commit of the newest release the integration tier proved on
prod. The rigs and their Forgejo repositories are in `rigs.conf` beside this
file. This file is the one definition of the gate; `README.md`, `rigs.conf`
and `main.go` point here.

Before running it by hand, installing it, or reading a failure it raised, read
`README.md` beside this file.

## What counts as proved on prod

A commit is proved on prod when its release run — `.forgejo/workflows/deploy.yml`
at a `v*` tag — succeeded and its `prod-integration` job, the app's integration
tier run against the prod URL after the release deployed, succeeded with it. A
staging run proves nothing here: a commit that only staging has shown is never
promoted. Neither is a `deploy.yml` run at a branch, even a branch whose name
starts with `v`: the run's ref must resolve to a tag that names the run's own
commit. The runs, tags and jobs come from the Forgejo Actions API through the
read-only viewer token, the same endpoints the dashboard's Deploys reader uses
(`internal/dashboard/deploys.go`); this plugin holds no GitHub credential, and
`gt promote` reads the rig's own deploy key.

The job name is the optional third column of `rigs.conf`, because a workflow
whose job is named otherwise would read as a repo that never goes green.

## What it promotes

The commit of the newest such release, and only when it is on the rig's Forgejo
`main` and ahead of the rig's `last_promoted` (the record `gt promote`, the
landing verdict and the tier sweep all write: `.runtime/red-main/<rig>.json`
under the town root). A commit equal to `last_promoted` is already on GitHub; one
behind it was proved after a newer commit had already been promoted, and pushing
it would roll GitHub back; one not on `main` is not what the rig ships. Each is
logged and skipped, and a run with nothing to do across every rig prints
`[plugin-result skipped]`, so the receipt says so.

`gt promote` is idempotent, so a second run of the same sha is harmless: it
reports the target already holds the commit.

## When it fails, and when it does not

A run that cannot read the viewer token, the rig list or the town root stops at
once and exits non-zero.

Otherwise every rig in `rigs.conf` is checked, and each rig's `gt promote`
refusal is either a routine skip or a hard failure. A routine skip is logged
with `gt promote`'s own line, and the rig counts as checked and is not a failure:

- exit 1, the commit is not on the rig's Forgejo `main` yet (`is not on rig`);
- exit 1, the tier sweep owns the promotion and has no fully green cycle yet, or
  only its last green commit is promotable (`is owned by the tier sweep`);
- exit 1, another promotion holds the rig's lock: retry later (`another promotion holds`);
- exit 2, the promotion target diverged and `gt promote` has raised
  `landing-promote-diverged:<rig>` for it (`the divergence is escalated`), so
  this plugin leaves the escalation to that one and a human reconciles once.

Every other non-zero exit is a hard failure of that rig. A rig that names no
`promote_target` is a hard failure, and so is one that names no `promote_key_file`:
a rig listed in `rigs.conf` that cannot promote is a misconfiguration, not a state
the next run resolves. So are a rejected push, a `gt promote` that cannot start,
and any message this list does not name, until someone classifies it.

The loop continues past a failing rig, whatever failed it: a `gt promote` hard
failure or a Forgejo read error. When every rig is checked, the run ends with
one error naming every failed rig, with its exit code or read error and its
cause line, one line each, in `rigs.conf` order. The daemon escalates that once
under `plugin:app-promote:failed`, which the next clean run closes. A run with
any failure never prints `[plugin-result skipped]`, even when no rig promoted.
