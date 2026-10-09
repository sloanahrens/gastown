+++
name = "app-promote"
description = "Promote each app rig's newest integration-green main commit to its GitHub target"
version = 1

[gate]
type = "cooldown"
duration = "10m"

[tracking]
labels = ["plugin:app-promote", "category:release"]
digest = true

[execution]
type = "script"
timeout = "5m"
+++

# App Promote

Advances each app rig's GitHub `main` to the newest commit on the rig's Forgejo
`main` that the deployed staging stack proved, so GitHub tracks what staging has
shown without waiting for a Gas Town landing (gt-5xrmp). The rigs it promotes
and their Forgejo repositories are in `rigs.conf` beside this file.

Before running it by hand, installing it, or reading a failure it raised, read
`README.md` beside this file.

## What counts as integration-green

A commit is integration-green when its `.forgejo/workflows/staging.yml` run on
`main` succeeded and its integration job — the `staging` job that applies the
built digest to the staging stack and checks its health — succeeded with it. The
runs, their refs and their jobs come from the Forgejo Actions API through the
read-only viewer token, the same endpoints the dashboard's Deploys reader uses
(`internal/dashboard/deploys.go`); this plugin holds no GitHub credential, and
`gt promote` reads the rig's own deploy key.

The job name is a column of `rigs.conf`, because a workflow whose integration
job is named otherwise would otherwise read as a repo that never goes green.

## What it promotes

The newest integration-green commit, and only when it is ahead of the rig's
`last_promoted` (the record `gt promote`, the landing verdict and the tier sweep
all write: `.runtime/red-main/<rig>.json` under the town root). A commit equal to
`last_promoted` is already on GitHub; one behind it was called green after a
newer commit had already been promoted, and pushing it would roll GitHub back.
Both are nothing to do, and a run with nothing to do across every rig prints
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
