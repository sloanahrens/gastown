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
file. This file is the one definition of the gate; `README.md`, `rigs.conf`,
`main.go` and `run.sh` point here.

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

A run that cannot read the viewer token, the rig list or Forgejo, or that
`gt promote` refused, exits non-zero with one line of cause; the daemon records
it and escalates once under `plugin:app-promote:failed`, which the next clean
run closes.

A diverged promotion target is not one of those: `gt promote` raises
`landing-promote-diverged:<rig>` for it, and this plugin prints the line and
leaves the escalation to that one, so a human reconciles once. A rig whose
`gt promote` refuses as not promotable — most often because its
`merge_queue.forgejo` block names no `promote_target`, or another promotion
holds the rig — is reported and skipped that run, because the cooldown retries
and an escalation should name a plugin that cannot do its job.
