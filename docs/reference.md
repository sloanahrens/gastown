# Gas Town Reference

Technical reference for Gas Town internals. Read the README first.

> For directory structure details, see [architecture.md](design/architecture.md).

## Beads Routing

Gas Town `gt` commands route beads work based on issue ID prefix. For direct
`bd` commands, run from the owning repository/root so the active `.beads`
directory matches the database you intend to touch.

```bash
bd -C ~/gt/greenplace/mayor/rig show gp-xyz  # Greenplace rig beads
bd -C ~/gt show hq-abc                       # Town-level beads
bd -C ~/gt/wyvern/mayor/rig show wyv-123     # Wyvern rig beads
```

**How it works**: Routes are defined in `~/gt/.beads/routes.jsonl`. Each rig's
prefix maps to its beads location (the rig's canonical clone at
`<rig>/mayor/rig` — a path kept from the retired mayor role, not a live agent).

| Prefix | Routes To | Purpose |
|--------|-----------|---------|
| `hq-*` | `~/gt/.beads/` | Cross-rig coordination, agent identity |
| `gp-*` | `~/gt/greenplace/mayor/rig/.beads/` | Greenplace project issues |
| `wyv-*` | `~/gt/wyvern/mayor/rig/.beads/` | Wyvern project issues |

Debug routing: `BD_DEBUG_ROUTING=1 bd -C <owning-root> show <id>`

`bd --global` is not Gas Town's town database. In Beads it targets a separate
shared-server database named `beads_global`; run `bd -C ~/gt ...` for
town-level Gas Town beads.

## Configuration

### Rig Config (`config.json`)

```json
{
  "type": "rig",
  "name": "myproject",
  "git_url": "https://github.com/...",
  "default_branch": "main",
  "beads": { "prefix": "mp" }
}
```

**Rig config fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `default_branch` | `string` | `"main"` | Default branch for the rig. Auto-detected from remote during `gt rig add`. Used as the landing target and as the base for polecats when no integration branch is active. |

### Settings (`settings/config.json`)

```json
{
  "theme": {
    "disabled": false,
    "name": "forest",
    "custom": {
      "bg": "#111111",
      "fg": "#eeeeee"
    },
    "role_themes": {
      "polecat": "rust",
      "crew": "none"
    }
  },
  "merge_queue": {
    "setup_command": "",
    "typecheck_command": "",
    "lint_command": "",
    "test_command": "",
    "build_command": "",
    "post_land_command": "bash scripts/post-land-shell.sh",
    "integration_branch_polecat_enabled": true
  },
  "dashboard": {
    "news": { "enabled": true }
  }
}
```

`merge_queue` is not exclusive to `settings/config.json`. It resolves across three
layers, lowest to highest precedence: the rig root `config.json` (the floor —
set once at onboarding), the repo-committed `.gastown/settings.json`, and this
file, `settings/config.json` (rig-local operator tuning, final override). Every
gate-command call site in `gt` reads all three through one resolver
(`rig.ResolveMergeQueueConfig`) so they can't drift out of sync.

**Theme fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `disabled` | `bool` | `false` | Disable tmux status/window theming for the rig |
| `name` | `string` | auto-assigned by rig name | Use a named built-in palette theme |
| `custom.bg` | `string` | unset | Custom tmux background color |
| `custom.fg` | `string` | unset | Custom tmux foreground color |
| `role_themes` | `map[string]string` | unset | Per-role overrides for `crew`, `polecat`; use `"none"` to disable theming for a role |

Theme resolution:
- No `theme` config: auto-assign a built-in palette theme by rig name
- `disabled: true`: skip both `status-style` and `window-style`
- `name`: use that built-in theme
- `custom`: use exact `{bg, fg}` colors
- `role_themes`: override role-specific sessions within the rig

Town-level role defaults live in `mayor/config.json` (like `mayor/rig`, a kept
path named for the retired mayor role) under:

```json
{
  "theme": {
    "disabled": false,
    "name": "forest",
    "custom": {
      "bg": "#111111",
      "fg": "#eeeeee"
    },
    "role_defaults": {
      "polecat": "rust",
      "crew": "none"
    }
  }
}
```

`role_defaults` supports `crew` and `polecat`.

**Dashboard fields** (`dashboard` block):

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `news.enabled` | `*bool` | `true` | The Hacker News pane above the dashboard's Machine panel. The hub reads the top five stories from the public Hacker News API into a snapshot it holds for five minutes, so Hacker News is read about that often and the page never calls it; a refresh that fails shows what it last read, dimmed. `false` leaves out both the pane and the fetcher. `gt dashboard` reads the key when it starts, so a change needs a restart. |

**Dashboard Jev lines.** The Cost panel keeps only the Jev spend row; the per-battery lines sit in their own JEV panel, folded until opened. Its title line totals the batteries, items and labeled items, states the scorecard's window and the read's age, and counts the questions earning, no-signal and not-yet. A battery whose report skipped lines says how many went unanswered. Before changing what a battery line shows or how the scorecard is read, run `gt dashboard --help`: `--jev-scorecard-cmd` there says what each line holds, how often it refreshes and what a failed run shows. The test-only `fixture` battery is left out of the listing and the dashboard's reads.

**Landing and gate fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `setup_command` | `string` | `""` | Setup/install command (e.g., `pnpm install`) |
| `typecheck_command` | `string` | `""` | Type check command (e.g., `tsc --noEmit`) |
| `lint_command` | `string` | `""` | Lint command (e.g., `eslint .`) |
| `test_command` | `string` | `""` | Test command to run. Empty = skip. `gt done`'s default test-verify gate inherits any leading `VAR=value` assignments from it, and writes the container opt-in off for the run unless the command turns `GT_TEST_DOCKER=1` on itself (gt-wx53) — see below. |
| `presubmit_command` | `string` | `""` | What `gt done` runs on the rebased branch before it pushes. Empty = `make presubmit` when a Go repo's Makefile has that target (lint, build, and the tests of the changed packages only), else `make gate`. The rig's Forgejo CI gate runs the full `make gate` on the candidate branch (gt-fn9e6.32). A command that turns `GT_TEST_DOCKER=1` on is refused, as for `test_command`. |
| `build_command` | `string` | `""` | Build command (e.g., `go build ./...`) |
| `post_land_command` | `string` | `""` | Run once per landing at the landed commit, and at any new tip of the default branch that no landing put there (a direct push). Red-main reads the units a red run blamed out of the run's own output: its failing Go packages, and the scripts the `cmd/tier-sweep` red summary line named. A command that runs the shell tests without that sweep names no failing script, so only a Go failure is attributable there. A red run whose sole landing since the last green commit can have moved what failed is reverted through the landing worker and its bead reopened for rework. A landing that cannot have moved it leaves the red-main bead to a human: a failing script needs one of the shell tier's inputs in the landing's diff, a failing Go package a Go input (gt-40so9). State in `.runtime/red-main/<rig>.status` and `<rig>.json`; the spec dispatcher reads the revert a rig has in flight out of `<rig>.json` and holds that rig's red-main beads until it lands (gt-zkdwt). Read from `settings/config.json` only. Empty disables it. |
| `max_ready_for_dispatch` | `int` | `0` | `gt sling` refuses new work while the rig has more ready MRs than this. 0 = guard off. |
| `followup_backpressure` | `int` | `40` | Open `om-followup` beads at which the follow-up feed (`landing.followups`) stops filing for this rig. The bound is counted as beads are filed, so a landing that splits into one bead per package stops at it rather than carrying the count past it. The findings it holds back are queued and filed by a later landing with room, so the count falling back under the bound resumes the feed rather than losing work; the queue itself is described under `landing.followups`. While it holds the feed back it logs how many findings were queued and raises one low attention item a day (gt-4ty22, gt-3arxj). Unset means 40. |
| `merge_strategy` | `string` | `""` | Passed to the polecat formula as `merge_strategy` |
| `require_review` | `*bool` | `false` | Passed to the polecat formula as `require_review=true` |
| `editorial.required` | `bool` | `false` | Read by the `editorial-required` doctor check only |
| `integration_branch_polecat_enabled` | `*bool` | `true` | Polecats auto-source worktrees from integration branches |
| `forgejo.remote_url` | `string` | `""` | The rig's Forgejo repository URL, where `gt done` and the landing worker push once the rig is cut over |
| `forgejo.gate_workflow` | `string` | `"gate"` | The candidate gate's workflow; the required commit-status context is derived from this name, not typed twice |
| `forgejo.bots` | `map[string]string` | `{}` | Bot role (`polecat`, `landing`, `registry`, `viewer`) to Forgejo login. The role names the token file `~/.config/gt/forgejo-<role>.env`, which is a host fact and never config |
| `forgejo.mirror_target` | `string` | `""` | The rig's read-only push mirror target (its GitHub repository). A mirror failure never blocks a landing |
| `forgejo.promote_target` | `string` | `""` | The GitHub repository URL a green `main` verdict fast-forwards to that verdict's commit. Empty leaves the rig with no promotion |
| `forgejo.promote_key_file` | `string` | `""` | The private deploy key ssh pushes with, mode 600. It is a path, never the key: the path reaches ssh alone and appears in no config, log or state. Empty disables the promotion |

The `forgejo.*` keys are operator-only: they resolve from the rig root
`config.json` floor and the rig-local `settings/config.json` override, and a
block in the repo-committed `.gastown/settings.json` is ignored with one
warning — the block names the remote a bot token is sent to and the logins the
landing creator check trusts, so merged repo content must not choose it. The
other `merge_queue` fields keep their three tiers; read
[Forgejo-primary landing](design/forgejo-primary-landing.md) before changing the
landing path, and call `rig.ResolveForgejoConfig` rather than reading the block
from one tier. A key inside the `forgejo` block that this binary does not
declare is ignored rather than a parse error, so a config written for another
gt version still loads; every other block stays strict.

**Landing fields** (`landing` block):

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `followups` | `bool` | `false` | File shaped `om-followup` beads, priority 3, for the minor findings of a landing om approved at or above the rig's `.om.json` threshold, whatever it scored. A passing review carries no major or critical finding for the feed to file — either refuses the landing back to its author (gt-oi4je) — so its minors are all there is. The findings go in one bead, or one bead per package when they reach more than one package or number more than six. A finding matching an existing follow-up by fingerprint, or by the same path with a similar title, comments `seen again on <bead>` there instead, and the work bead gets one comment naming the beads filed. A landing whose own bead wears `om-followup` files nothing, so a fix's minors never feed the feed again. A retry of a landing attempt that filed before failing refiles none of what it filed, and files the packages that attempt never reached — it says nothing to its own beads, since `seen again` is for a finding another landing raised. `merge_queue.followup_backpressure` open `om-followup` beads stop it; the findings it holds back are queued in the landing's review-log line and filed by the next landing with room, oldest landing first, and a queued finding a bead already covers (open or closed) is neither filed nor commented on. A queue line the feed has emptied is dropped from the log, and a log line too long to read is skipped rather than stopping the queue; the queue is read before the work bead, so a landing with nothing to file and no queue behind it costs one read (gt-3arxj). Every read, comment and file is fail-soft and bounded at 20s: the landing is recorded first, and the feed never changes its result. |

Read from the rig's own `settings/config.json` only, like `rigPostLandCommand`:
the feed files beads, and merged repo content must not choose that — nor the
`merge_queue.followup_backpressure` bound the
feed stops at, which the daemon reads from the same file.

A file that still sets the retired `landing.om_queue` loads: the key is declared
so strict decoding does not refuse the file, and no reader reads it (gt-50k2e).

**The `forgejo` block is mandatory for any rig that lands.** It is the rig's
only landing path: the local gate and the force-with-lease push to the target
were removed once every rig was cut over (gt-fn9e6.32). A rig with no block
gets no landing worker at all — construction fails closed with an escalation
naming the rig and its `settings/config.json` — rather than silently falling
back to a path that no longer exists. `scripts/forgejo-resync.sh` is the
recovery if the block must be removed or a Forgejo repository has to be rebuilt
from GitHub; there is no rollback to a local gate.

Keys removed in gt-5nlvq (`enabled`, `run_tests`, `on_conflict`, `poll_interval`,
`batch_*`, `test_verify_*` and the other refinery-era keys) now fail strict
decoding; `gt doctor fix deprecated-merge-queue-keys` deletes them.

**Landing timeouts.** The candidate gate waits up to 20 minutes for the
required commit-status context to report; a wait that outlives that window is
CI silence, an infrastructure outcome that retries rather than rejecting the
work. A status read that returns an error is not a verdict either: the gate
retries it, up to three consecutive reads (a good read resets the count), so a
transient 502 or timeout does not discard the candidate (gt-394h5). A landing's
deadline is that CI wait plus the rig's
`patrols.landing_worker.om_timeout` (5 minutes by default) plus 5 minutes of
merge slack — 30 minutes at the defaults. A deadline hit is infrastructure, as
CI silence is.

See [Integration Branches](concepts/integration-branches.md) for integration branch details.

**Container opt-in and the container-gate slot (`gt done`'s gate).** Container-backed
tests are opt-in (`GT_TEST_DOCKER=1`, see `internal/testutil`), and a run that can start
one must hold the town-wide container-gate slot. `gt done`'s default test-verify gate
runs the rig's `presubmit_command` (for gastown, `make presubmit`) with the opt-in written **off**,
so it takes no slot. `make presubmit` tests only the changed packages; the rig's Forgejo CI
gate runs the full `make gate` on the candidate branch. Both recipes write `GT_TEST_DOCKER=0` themselves: the
unit tier never starts a container, whoever runs it. The container suites run post-merge
in `make test-integration`, and the shell tests after the gate in `make test-slow` (see [Testing](testing.md), "The gate"). A rig that wants its
container suite verified at `gt done` asks for it in its own command
(`test_command: "GT_TEST_DOCKER=1 make <target>"`), and the gate honours that and takes a
slot for it. The session's own exported value is deliberately not an input: the slot
decision and the environment the suite reads are one fact (gt-0hbm).

**Presubmits are capped townwide.** Each `gt done` runs its local gate on the host, and
three to five at once starved it (2026-10-20: host idle 0-2%, `bd` reads from 0.3 s to
5-12 s). A presubmit pool of its own caps them at `operational.daemon.presubmit_pool`
(default `2`). A `gt done` past the cap waits for a free slot inside the gate's 60-minute
budget, logs one line once the wait passes 60 s, and shows in `gt slot status` under
"Presubmit pool". It never takes a container-gate slot, and a landing never waits on it.

**Whole-tree runs are capped (gt-dhcmp).** A whole-tree test run is the heaviest thing a
seat starts, and several at once starve the landing gate: on 2026-10-03 whole-suite runs
from several seats pushed one landing's gate to 5m02s against a 26-39s norm. The
container-gate pool now caps the full-suite class at `container_gate.max_full_suites`
concurrent holders townwide (default `1`; `0` disables the cap). A role is full-suite
class when it ends in `/full-suite`, `/tier-sweep` or `/flake-sweep`, so a session
wrapping its own whole-tree run asks for the cap by naming its role that way:

```
gt slot run --role gastown/crew/sloan/full-suite -- make test
```

A second whole-tree start waits for the running one instead of competing for a free
slot, and reports that wait like any other: `full_suite_held` in `gt slot status`, with
the run it ceded to named. Package-scoped suites (`go test ./internal/x/...`, a
`-run`-filtered run, `make presubmit`) are unaffected. The landing gate is never capped,
never counted against the cap and never waits on it: a gate is not full-suite class, so
it takes its own slot — a gate-reserved one where the pool reserves any, any free one
otherwise — and a gate started while a whole-tree run holds a slot proceeds at once. The daemon's tier sweep already
takes the token as `<rig>/tier-sweep`; run the flake sweep as
`gt slot run --role <rig>/flake-sweep -- bash scripts/flake-sweep.sh [ITER] [CONC]`.

**A post-landing suite yields to an install in flight (gt-t135z).** The post-landing
suite — `<rig>/post-land`, the red-main detector a landing runs once merged — is the one
role class that keeps out of the gate slot while an install is pending. Before changing
what that yield does, read the install protocol in [INSTALLING.md](INSTALLING.md): it
says when a marker stands, how long the yield may last and which roles it never holds
back. The wait ends early once the install's build (`gastown/rebuild-gt`) holds a slot,
its reservation being in place by then. `gt slot status` lists the wait among its recent
acquisitions as `install_pending`, with the pid that wrote the marker (`install_pid` in
`--json`).

Run `gt done` **once**, then leave it alone: the gate takes minutes. If it exits non-zero it
names what failed (exit codes 10-16, `gt done --help`) and the session stays up; fix what it
names and run `gt done` once more. A failure you believe your change did not cause goes in a
bead comment, then `gt escalate -s medium`. No flag skips a polecat's gate.

**Crew submission (gt-3e7tk).** A crew session submits with the same command. Commit on a
crew branch, push it (`git push origin HEAD:<branch>`), then run `gt done --bead <id>`.
The bead comes from `--bead`, else from a branch name carrying a routed bead id;
`crew/<user>/<slug>` usually carries none, so pass `--bead`. `gt done` refuses unless
`origin/<branch>` is at HEAD and HEAD is ahead of the target, runs `make presubmit`
(skip it with `--pre-verified` when you already ran it), then writes the comment
`Submitted for landing: <branch> @ <sha> onto <target>`, the READY TO LAND block and the
`gt:ready-to-land` label. It pushes nothing and rebases nothing.
`BD_ACTOR` falls back to `git config user.name`. A polecat's `gt done` refuses
`--pre-verified`.

**The submission's actor (gt-2ge3z).** The landing worker refuses a submission recorded under
the human's own identity, the town's overseer name, and leaves one comment naming that actor:
an agent must submit it, for example the overseer from its crew session. The actor is the
author of the newest `Submitted for landing` comment naming the submission's branch and head,
which is what a crew session records, else the READY TO LAND block's `Worker` line, which a
polecat's `gt done` writes. A submission whose actor cannot be read lands nothing, and neither
does any submission on a town whose overseer identity cannot be read: that town builds no
landing worker until the identity is fixed.

**Re-queueing a rejected landing (gt-3e1z4).** A landing the worker refuses labels the
bead `rework` (removing `gt:ready-to-land`) and appends a `MERGE REJECTION` note naming
the head it refused. A bead the human-only label holds (the old name; it still
holds until it is retired) keeps its assignee: its branch comes from a person,
so the refusal goes back to the crew that submitted it rather than onto the
board (gt-me800). A bead a bird claimed — any assignee but a
`<rig>/polecats/<name>` seat — keeps its assignee and `in_progress` status, and the owner
hears of the refusal from a `rejected:<bead>` attention item instead (gt-h4cvi). When the
polecat's branch still sits on that head the
rejection came from something other than the diff, and nothing needs to change.
`gt land requeue <bead> --reason "<why>"` removes `rework`, restores `gt:ready-to-land`
and records the operator, the reason and the head as a bead comment, so the worker gates
and merges the same head again. It refuses when the head changed (a normal `gt done`
resubmission, which re-gates), when the bead is not in a rejected state, and when the
branch is gone from the remote; it never re-queues a head that has not passed presubmit.
`gt done` on a rework bead still on the rejected head does not resubmit: it stands down,
leaves the bead open, and tells the polecat to ask for a `gt land requeue` if the
rejection was environmental, or to make a new commit otherwise.

**Recording a decision on a review finding (gt-yphcl).** Each om round's findings are kept
in `rounds-<bead>.json` under the rig's om directory (`.runtime/landing-om/<rig>/`), with
what became of each. A comment on the work bead records a decision about one, and only the
overseer or Sloan may write it: a polecat describes the disagreement in a plain comment and
lets them decide.

    ACCEPTED-RISK: F2 the mount is approved by design 5.5
    REBUTTAL: F3 the guard runs on the write path too

`ACCEPTED-RISK:` records a decision already made, so a finding that restates it is info.
`REBUTTAL:` records that the finding is wrong. Each line of the comment names one finding
by its exact id, the word after the marker, so a comment carries one line per finding. A
line that only mentions an id in prose settles nothing, and a file name settles nothing:
the ledger's identity for a finding is the (file, id) pair, and a comment names only the id.
Wiring the settlement into the landing review is gt-3l5qy's; until it lands, every finding
the ledger records stays open.

Never poll the slot, and never script a retry around `gt done` or `gt slot`: a
polling loop holds the gate every other agent is queued behind, one pass at a
time (gt-7dxw). The dangerous-command guard refuses the loop shape — a
`for`/`while`/`until` block that closes around `gt done` or a read-only
`gt slot status`, a repeat wrapper whose own payload runs either, or a heredoc
writing such a script to a file — in every role and every directory. Repeating
`gt slot run --role <rig>/<you> -- <command>`, which takes and releases the slot,
is a bounded sequence rather than polling, and the guard allows it. The guard is
a seat belt, not a cage: it
does not read a script the command merely executes (`bash /tmp/retry.sh`) and
cannot see one written through `printf` or `echo`, so this rule, not the guard,
is what binds.

**Docker VM size and the Dolt data dir.** One full `-p=8` suite starts about 11 Dolt test
containers, and the gate slot pool admits more than one suite. On Docker Desktop, give the VM at
least 16 GiB (Settings → Resources); `gt doctor`'s `container-capacity` check warns below that.
Test containers keep their Dolt data on tmpfs (`/var/lib/dolt`, capped at 2 GiB each) so each
migration's commit does not fsync through to the host disk. Set `GT_TEST_DOLT_TMPFS=0` to use the VM disk
instead, for a Docker runtime that cannot mount tmpfs there. Measurements:
`docs/plans/2026-09-24-test-suite-concurrency-design.md`.

### Spec Dispatch (`polecat_pool`)

The spec dispatcher's policy lives in the town's `settings/config.json`, under
`polecat_pool`, beside the seats it fills: `max_priority` is the ceiling on a
candidate bead's number (the daemon's idle-seat check counts to the same
number), and `shape_gate` is what a dispatch does with a candidate's shape lint
(`gt spec lint`): off, warn or refuse. `seats` is the seat map, one entry per
agent: `cap` is the live polecats allowed on that agent (0 closes the seat),
`closed` is an optional daily `HH:MM-HH:MM` window in `timezone` during which
the seat takes no new spawn, and `names` is an optional list of polecat names
for the seat. `min_spawn_gap` staggers spawns. `max_total` caps live polecats
across every seat, for a town whose agents are each inside their own cap and
over the number wanted; `gt pool set-total <n>` sets it, and an absent key (or 0)
is no total cap. `gt pool show` prints the seats, `gt pool set <agent>=<cap>`
changes them.

The two-seat keys the map replaced no longer load (gt-mukgk): `polecat_pool.agent`,
`max_seats`, `overflow_agent`, `max_overflow`, `pro_agent`, `pro_max` and
`pro_label`, `daemon.patrols.spec_dispatch.hooked_agent`, `max_hooked` and
`prefer_hooked`, `role_agents.polecat`, and `agent` on a `dispatch: "spec"`
entry in `scheduled_slings`. A file that holds one fails to load with a message
that names the key and what replaced it; `gt pool migrate` converts the file
(`--dry-run` prints the changes and writes nothing). Pin a bead to a model with
an `agent:<name>` label; a scheduled spec entry pins its run beads with
`run_agent` (see [the spec dispatcher guide](guides/spec-dispatcher.md)).
`gt config get --help` lists the keys with their defaults; `gt config get
polecat_pool.<key>` prints the effective value. Edit the file by hand, or `gt
config set polecat_pool.<key> <value>`.

The keys were the seat-refill plugin's before it was deleted (gt-4k3fj.8.8),
which is why a few names say what they no longer do: `mode`, `top_candidates`,
`empty_seconds`, `nudge_seconds` and `dispatch_empty_seconds` tuned the plugin's
nudge and its delayed fill. Nothing reads them now; they are accepted and
ignored, and kept so an older settings file still loads.

A value the dispatcher cannot act on is refused rather than ignored: `gt config
set` rejects it at the write, and the daemon refuses to start from a file
carrying one, naming the file and the key.

`settings/daemon.env` is for secrets and process environment (below), and the
`daemon.env` map in `settings/config.json` is the ambient environment the daemon
exports to every session it spawns. A dispatch policy put in either place is
invisible to `gt config`, unvalidated, and applied to every session rather than
to the dispatcher that reads it — which is how raising the ceiling to P3 took
two tries (gt-y3pgh.12).

`docs/guides/spec-dispatcher.md` is the behavioral guide: candidates, the shape
gate, the seat table and how to turn the ticker off.

### Disk Maintenance

Registry retention and the `disk_maintenance` patrol are switched by two
environment knobs and one key under `daemon` in `settings/config.json`:

| Knob | Default | Read by |
|------|---------|---------|
| `POLECAT_IMAGE_REGISTRY_KEEP` | `5` | the newest per-commit tags kept by the plugin's retention, `gt maint` and the patrol |
| `POLECAT_IMAGE_REGISTRY_PRUNE` | `dry-run` | the `polecat-image` plugin only: `off`, `dry-run` or `apply` |
| `daemon.patrols.disk_maintenance` | disabled; once enabled, `{"interval": "daily", "window": "04:30", "mode": "dry_run", "targets": {"civol": {"interval": "2h"}}}` (`mode` may be `apply`) | the daemon patrol |

A `targets` entry gives one target a cadence of its own, such as civol's `2h`
for the CI cache volumes; a target without one runs in the daily window. An
interval that is not a Go duration of at least a minute, or a target name the
patrol does not know, is refused when the config is read.

The two environment knobs are read from the environment of the run: the
daemon's for the plugin and the patrol, the shell's for a hand-run `gt maint`.
None of the three applies anything by default. A value the run cannot act on
refuses the run rather than reading as "keep nothing" or "prune nothing".

[The disk maintenance guide](guides/disk-maintenance.md) says what a run
deletes and keeps, when it refuses, and how to read the ledger.

### Daemon Environment (`settings/daemon.env`)

Optional. One `KEY=VALUE` pair per line; blank lines and lines starting with
`#` are ignored:

```
# Fix for the CLT/SDK linker break under cmux (om-xij)
CMUX_CLAUDE_HOOKS_DISABLED=1
SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk
DEVELOPER_DIR=/Library/Developer/CommandLineTools
```

`MacOSX.sdk` above is a symlink to whichever SDK Command Line Tools installed
most recently, so a CLT whose SDK declares architectures `ld` rejects makes
every cgo build fail with `tapi error: malformed file`. When that happens, run
`gt doctor`: its `macos-sdk` check names the offending stubs, and the fix is to
pin `SDKROOT` to a versioned SDK that passes it (gt-1a0t).

A manually-started daemon (`gt daemon start`) inherits the operator's shell
environment. A daemon launched by an external supervisor (launchd on macOS,
systemd on Linux, via `gt daemon enable-supervisor`) starts with a bare
environment instead, so host-specific fixes like the ones above would
otherwise be silently lost. `gt daemon enable-supervisor` reads this file (if
present) and renders each pair into the launchd plist's
`EnvironmentVariables` / the systemd unit's `Environment=` directives.
`GT_TOWN_ROOT` is always set from the resolved town root and cannot be
overridden here. Changes only take effect on the next `gt daemon
enable-supervisor` run (re-run it after editing the file).

#### Secrets by reference

Agent tokens live in this file, at mode 0600, and an agent's `env` block in
`settings/config.json` references them by name with the braced form:

```json
"agents": {
  "deepseek-flash": {
    "env": {"ANTHROPIC_AUTH_TOKEN": "${DEEPSEEK_FLASH_ANTHROPIC_AUTH_TOKEN}"}
  }
}
```

```
DEEPSEEK_FLASH_ANTHROPIC_AUTH_TOKEN=sk-...
```

A session's startup command does not carry the value: it reads the named
entry from `settings/daemon.env` when it runs, so the token appears in no
process's argv and in no tmux pane start command. A reference that
`settings/daemon.env` does not define is expanded from the spawning process's
environment, as before; one that neither defines stops the spawn. A token in
this file is not rendered into the launchd plist or systemd unit (a value
that looks like a token is left out), so the daemon's own environment never
hands one provider's token to every agent. Name entries after the agent, not
after the variable the agent reads.

The spawning process's own credentials are never forwarded to a session:
`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`,
`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`,
`AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_FOUNDRY_API_KEY`,
`CLAUDE_CODE_CLIENT_KEY_PASSPHRASE`, and an `HTTP(S)_PROXY` URL that carries
`user:password@`. A forwarded value would land in `tmux new-session -e`, the
startup command and the handoff respawn command. An agent that needs one names
it as a `${VAR}` reference to `settings/daemon.env`. Plain provider settings
(model names, `AWS_PROFILE`, `AWS_REGION`, the Bedrock/Vertex/Foundry
switches, credential file paths) are still forwarded.

A literal token in an agent's env (a value with a known token prefix such as
`sk-`, or a 16+ character value for a key naming a credential) draws a
warning: `gt doctor` (`town-config-secrets`) lists its key path, and the
town-running commands and the daemon print one line at start. Values are
never printed. To move them:

```
gt config secrets migrate --dry-run     # key paths and variable names only
gt config secrets migrate               # writes daemon.env (0600), rewrites config to ${...}
gt doctor                               # town-config-secrets is OK
gt config set secrets.refuse_literals true
```

With `secrets.refuse_literals` set, a literal token is a load error: the
daemon, `gt up` and every session start refuse, naming the key path. `gt
config set` refuses to turn it on while a literal remains.

### Runtime (`.runtime/` - gitignored)

Process state, PIDs, ephemeral data.

### Rig-Level Configuration

Rigs support layered configuration through:
1. **Wisp layer** (`.beads-wisp/config/`) - transient, local overrides
2. **Rig identity bead labels** - persistent rig settings
3. **Town defaults** (`~/gt/settings/config.json`)
4. **System defaults** - compiled-in fallbacks

#### Polecat Branch Naming

Configure custom branch name templates for polecats:

```bash
# Set via wisp (transient - for testing)
echo '{"polecat_branch_template": "adam/{year}/{month}/{description}"}' > \
  ~/gt/.beads-wisp/config/myrig.json

# Or set via rig identity bead labels (persistent)
gt bead update gt-rig-myrig --add-label=polecat_branch_template:adam/{year}/{month}/{description}
```

**Template Variables:**

| Variable | Description | Example |
|----------|-------------|---------|
| `{user}` | From `git config user.name` | `adam` |
| `{year}` | Current year (YY format) | `26` |
| `{month}` | Current month (MM format) | `01` |
| `{name}` | Polecat name | `alpha` |
| `{issue}` | Issue ID without prefix | `123` (from `gt-123`) |
| `{description}` | Sanitized issue title | `fix-auth-bug` |
| `{timestamp}` | Unique timestamp | `1ks7f9a` |

**Default Behavior (backward compatible):**

When `polecat_branch_template` is empty or not set:
- With issue: `polecat/{name}/{issue}+{timestamp}`
- Without issue: `polecat/{name}-{timestamp}`

**Example Configurations:**

```bash
# GitHub enterprise format
"adam/{year}/{month}/{description}"

# Simple feature branches
"feature/{issue}"

# Include polecat name for clarity
"work/{name}/{issue}"
```

## Formula Format

```toml
formula = "name"
type = "workflow"           # workflow | expansion | aspect
version = 1
description = "..."

[vars.feature]
description = "..."
required = true

[[steps]]
id = "step-id"
title = "{{feature}}"
description = "..."
needs = ["other-step"]      # Dependencies
```

**Composition:**

```toml
extends = ["base-formula"]

[compose]
aspects = ["cross-cutting"]

[[compose.expand]]
target = "step-id"
with = "macro-formula"
```

## Molecule Lifecycle

> For the full lifecycle diagram and detailed command reference, see [concepts/molecules.md](concepts/molecules.md).

**Summary**: Formula (TOML) --`bd cook`--> Protomolecule --`bd mol pour`--> Mol (persistent) or Wisp (ephemeral) --`bd mol squash`--> Digest.

| Operation | bd (data) | gt (agent) |
|-----------|-----------|------------|
| Cook/pour/wisp | `bd cook`, `bd mol pour/wisp` | — |
| Squash/burn | `bd mol squash/burn <id>` | `gt mol squash/burn` (attached) |
| Navigate | `bd mol current`, `bd mol show` | `gt hook`, `gt mol current` |
| Attach | — | `gt mol attach/detach` |

## Agent Lifecycle

### Polecat Shutdown

```
1. Work through formula checklist (shown inline by gt prime)
2. gt done gates and pushes the branch and marks the bead ready to land
3. gt done exits the session
4. The daemon's landing worker lands the branch and closes the bead
```

### Session Cycling

```
1. Agent notices context filling
2. gt handoff (sends mail to self)
3. Manager kills session
4. Manager starts new session
5. New session reads handoff mail
```

## Environment Variables

Gas Town sets environment variables for each agent session via `config.AgentEnv()`.
These are set in tmux session environment when agents are spawned.

### Core Variables (All Agents)

| Variable | Purpose | Example |
|----------|---------|---------|
| `GT_ROLE` | Agent role, compound `<rig>/polecats/<name>` or `<rig>/crew/<name>` | `gastown/polecats/toast` |
| `GT_TOWN_ROOT` | Town root directory | `/home/user/gt` |
| `BD_ACTOR` | Agent identity for attribution | `gastown/polecats/toast` |
| `GIT_AUTHOR_NAME` | Commit attribution (the agent name alone, without rig or role) | `toast` |
| `BEADS_DOLT_AUTO_START` | `0`: agents never auto-start a Dolt server; `BEADS_DIR` is cleared | `0` |

### Rig-Level Variables

| Variable | Purpose | Roles |
|----------|---------|-------|
| `GT_RIG` | Rig name | polecat, crew |
| `GT_POLECAT` | Polecat worker name | polecat only |
| `GT_CREW` | Crew worker name | crew only |
| `BEADS_AGENT_NAME` | Agent name for beads operations | polecat, crew |

### Other Variables

| Variable | Purpose |
|----------|---------|
| `GT_TMUX_SOCKET` | tmux socket the town's agents live on |
| `CLAUDE_CONFIG_DIR` | Custom Claude settings directory |

### Environment by Role

| Role | Key Variables |
|------|---------------|
| **Polecat** | `GT_ROLE=<rig>/polecats/<name>`, `GT_RIG=<rig>`, `GT_POLECAT=<name>`, `BD_ACTOR=<rig>/polecats/<name>` |
| **Crew** | `GT_ROLE=<rig>/crew/<name>`, `GT_RIG=<rig>`, `GT_CREW=<name>`, `BD_ACTOR=<rig>/crew/<name>` |

### Doctor Check

The `gt doctor` command verifies that running tmux sessions have correct
environment variables. Mismatches are reported as warnings:

```
⚠ env-vars: Found 3 env var mismatch(es) across 1 session(s)
    gt-crew-sloan: missing GT_TOWN_ROOT (expected "/home/user/gt")
```

Fix by restarting sessions: `gt shutdown && gt up`

## Agent Working Directories and Settings

Each agent runs in a specific working directory and has its own Claude settings.
Understanding this hierarchy is essential for proper configuration.

### Working Directories by Role

| Role | Working Directory | Notes |
|------|-------------------|-------|
| **Crew** | `~/gt/<rig>/crew/<name>/` | Persistent human workspace clone |
| **Polecat** | `~/gt/<rig>/polecats/<name>/<rig>/` | Polecat worktree (ephemeral sandbox) |

Note: The per-rig `<rig>/mayor/rig/` directory is NOT a working directory—it's
a git clone that holds the canonical `.beads/` database for that rig.

### Settings File Locations

Settings are installed in gastown-managed parent directories and passed to
Claude Code via the `--settings` flag. This keeps customer repos clean:

```
~/gt/
└── <rig>/
    ├── crew/.claude/settings.json           # Shared by all crew members
    └── polecats/.claude/settings.json       # Shared by all polecats
```

The `--settings` flag loads these as a separate priority tier that merges
additively with any project-level settings in the customer repo.

### CLAUDE.md

`~/gt/CLAUDE.md` is a minimal identity anchor that prevents agents from losing
their Gas Town identity after context compaction or new sessions.

Full role context (~300-500 lines per role) is injected ephemerally by `gt prime`
via the SessionStart hook. Each polecat worktree also gets a `CLAUDE.md` carrying
the `gt done` lifecycle contract (`CLAUDE.local.md` when the repo already tracks a
`CLAUDE.md`).

**Why so little else on disk?**
- Claude Code traverses upward from CWD for CLAUDE.md — all agents under `~/gt/` find the town-root file
- The real context comes from `gt prime`, making on-disk bootstrap pointers redundant

### Customer Repo Files (CLAUDE.md and .claude/)

Gas Town no longer uses git sparse checkout to hide customer repo files. Customer
repositories can have their own `.claude/` directory and `CLAUDE.md` — these are
preserved in all worktrees (crew, polecats, mayor/rig).

Gas Town's context comes from the town-root `CLAUDE.md` identity anchor
(picked up by all agents via Claude Code's upward directory traversal),
`gt prime` via the SessionStart hook, and the customer repo's own `CLAUDE.md`.
These coexist safely because:

- **`--settings` flag provides Gas Town settings** as a separate tier that merges
  additively with customer project settings, so both coexist cleanly
- **`gt prime` injects role context** ephemerally via SessionStart hook, which is
  additive with the customer's `CLAUDE.md` — both are loaded
- Gas Town settings live in parent directories (not in customer repos), so
  customer `.claude/` files are fully preserved

**Doctor check**: `gt doctor` warns if legacy sparse checkout is still configured.
Run `gt doctor fix sparse-checkout` to remove it. Tracked `settings.json` files in worktrees are
recognized as customer project config and are not flagged as stale.

### Settings Inheritance

Claude Code's settings are layered from multiple sources:

1. `.claude/settings.json` in current working directory (customer project)
2. `.claude/settings.json` in parent directories (traversing up)
3. `~/.claude/settings.json` (user global settings)
4. `--settings <path>` flag (loaded as a separate additive tier)

Gas Town uses the `--settings` flag to inject role-specific settings from
gastown-managed parent directories. This merges additively with customer
project settings rather than overriding them.

### Settings Templates

Gas Town installs one base hooks config for every role, then layers per-role
overrides. The base config is `DefaultBase` in `internal/hooks/config.go`; read
it there for the commands each hook event runs.

### Troubleshooting

| Problem | Solution |
|---------|----------|
| Agent using wrong settings | Check `gt doctor`, verify `.claude/settings.json` in role parent dir |
| Settings not found | Run `gt install` to recreate settings, or `gt doctor fix <check>` |
| Source repo settings leaking | Run `gt doctor fix sparse-checkout` to remove legacy sparse checkout |

## CLI Reference

### Town Management

```bash
gt install [path]            # Create town
gt install --git             # With git init
gt doctor                    # Read-only health check
gt doctor fix <check>        # Repair one named check
```

### Configuration

```bash
# Agent management
gt config agent list [--json]     # List all agents (built-in + custom)
gt config agent get <name>        # Show agent configuration
gt config agent set <name> <cmd>  # Create or update custom agent
gt config agent remove <name>     # Remove custom agent (built-ins protected)

# Default agent
gt config default-agent [name]    # Get or set town default agent

# Town settings by dot-notation key (gt config get --help lists the keys)
gt config get polecat_pool.max_priority     # Effective value: the file's, else
                                            # the default
gt config set polecat_pool.max_priority 3   # Refused if the value cannot work
```

**Built-in agents**: `claude`, `groq-compound` (the Claude CLI over Groq). Every
agent runs the Claude Code CLI or a wrapper that execs it, and gets the town's
managed settings through `--settings`.

**Custom agents**: Define per-town via CLI or JSON:
```bash
gt config agent set claude-glm "claude-glm --model glm-4"
gt config agent set claude "claude-opus"  # Override built-in
gt config default-agent claude-glm       # Set default
```

**Advanced agent config** (`settings/agents.json`):
```json
{
  "version": 1,
  "agents": {
    "claude-haiku": {
      "command": "claude",
      "args": ["--model", "haiku", "--dangerously-skip-permissions"]
    }
  }
}
```

**Rig-level agents** (`<rig>/settings/config.json`):
```json
{
  "type": "rig-settings",
  "version": 1,
  "agent": "claude-haiku",
  "agents": {
    "claude-haiku": {
      "command": "claude",
      "args": ["--model", "haiku", "--dangerously-skip-permissions"]
    }
  }
}
```

**Agent resolution order**: rig-level → town-level → built-in presets.

### Rig Management

```bash
gt rig add <name> <url>
gt rig list
gt rig list --json
gt rig remove <name>
```

`gt rig list --json` emits one object per rig. Fields relevant to plugin
consumers:

| Field       | Type   | Source |
|-------------|--------|--------|
| `name`      | string | rig name |
| `status`    | string | operational / stopped / … |
| `repo_path` | string or null | `Rig.RepoPath()` — the first of the rig root, `<rig>/mayor/rig`, `<rig>/refinery/rig` that is a git worktree root; `null` if none |

`repo_path` is the field a script that runs git in each rig's repository
consumes. When it is `null` or missing, such a script must fail loudly rather
than no-op (gt-chqi, gt-xxwx). The daemon's git_hygiene patrol resolves its
repository through `gitHygieneRepoPath` in `internal/daemon/git_hygiene.go`,
which prefers the rig's shared bare repo (`.repo.git/`) to what this field
reports.

### Repository Guards

`gt repoguard` holds the current directory's repository to a shrinking
`.repoguard-allowlist` at its root; `gt repoguard --help` carries the line
format, the flags and the exit codes.

It exists for repos with no Go module to carry a test of their own, so the rule
that a script is debt travels with the binary (gt-7zfre); a rig opts in by
putting the command in its gate. gastown's own scripts are held instead by
`TestIntegrationNoNewShellScripts` in `internal/repoguard`, which reads
`shell-scripts.txt`: different list, different file, not shared.

### Work Tracking (Primary Dashboard)

```bash
gt ready                                # Beads with no blockers, town-wide
gt ready --rig=gastown                  # One rig's unblocked beads
gt show <bead-id>                       # Status, dependencies, assignee
gt agents                               # Live agent sessions
gt polecat list <rig>                   # Polecats in a rig (hidden from gt agents)
```

Note: "Swarm" is ephemeral (workers on an epic's issues).

### Work Assignment

```bash
# Standard workflow: order the batch, then sling each bead
gt bead dep add gt-def gt-abc            # gt-def waits for gt-abc to land
gt sling gt-abc <rig>                    # Assign to polecat
gt sling gt-abc <rig> --agent claude-haiku  # Override the agent for this sling/spawn
gt sling <proto> --on gt-def <rig>       # With workflow template

# A sling does not create a convoy: the spec dispatcher is the only automatic
# dispatcher, and a hand-slung bead is tracked by its own status and assignee.
gt sling <bead> <rig>                    # Dispatch to a polecat in the rig
```

Agent overrides:

- `gt sling <bead> <rig> --agent <alias>` honours a `polecat_pool` seat or refuses the sling; a seat that is full never spends on the other agent instead.
- `gt crew start <name> --agent <alias>` and `gt crew at <name> --agent <alias>` override the crew worker runtime.

### Communication

```bash
gt mail inbox
gt mail read <id>
gt mail send <addr> -s "Subject" -m "Body"
gt mail send overseer -s "..."   # To overseer
```

### Escalation

```bash
gt escalate "topic"              # Default: MEDIUM severity
gt escalate -s CRITICAL "msg"    # Urgent, immediate attention
gt escalate -s HIGH "msg"        # Important blocker
gt escalate -s MEDIUM "msg" -m "Details..."
```

See [escalation.md](design/escalation.md) for full protocol.

### Sessions

```bash
gt handoff                   # Request cycle (context-aware)
gt done                      # Submit branch and terminate (polecats)
gt session stop <rig>/<agent>
gt peek <agent>              # Check health
gt nudge <agent> "message"   # Send message to agent
```

**Session Discovery**: Each session has a startup nudge that becomes searchable
in Claude's `/resume` picker:

```
[GAS TOWN] recipient <- sender • timestamp • topic[:mol-id]
```

Example: `[GAS TOWN] gastown/crew/gus <- human • 2025-12-30T15:42 • restart`

**IMPORTANT**: Always use `gt nudge` to send messages to Claude sessions.
Never use raw `tmux send-keys` - it doesn't handle Claude's input correctly.
`gt nudge` uses literal mode + debounce + separate Enter for reliable delivery.

### Emergency

```bash
gt estop [--rig <rig>]       # Stop dispatch and restarts; running sessions finish
gt estop status              # Show town and per-rig E-stops
gt thaw [--rig <rig>]        # Clear the E-stop
gt kill-all [--rig <rig>]    # Show what kill-all would end
gt kill-all --yes [-r why]   # E-stop, then kill every agent session
gt down                      # Stop all Gas Town services
```

An E-stop (`<town>/ESTOP`, or `<town>/ESTOP.<rig>`) is read in two places
only: the dispatch hold, which every automatic dispatcher and `gt sling`
check, and the supervisor's Kill and Restart guard, which every automated
kill and restart goes through. Nothing is signaled; running sessions
finish or idle and learn of the stop from their mail-check reminder.
`gt kill-all` is the explicit way to end them: it sets the E-stop, then
kills each agent seat through the supervisor (the one kill an E-stop or a
parked seat does not refuse), logging actor and reason in
`.runtime/supervisor/actions.jsonl`. Crew sessions are skipped unless
`--crew` is given; the overseer session is never killed.

Before writing or releasing a dispatch hold — a marker on a bead's own record
that parks it out of automatic dispatch — read [Dispatch
holds](concepts/dispatch-holds.md): it states the write form of each marker and
the path that reads it.

### Attention Queue

`gt attention` reads the queue of conditions the daemon maintains for human or
overseer judgment, and acknowledges an item.

```bash
gt attention                 # the queue, highest needs_human score first (risk-path rows: see below)
gt attention --all           # include acked items
gt attention --json          # state.json
gt attention -f              # stream new transitions as they append
gt attention --follow --wake-only   # only the events that need a decision
gt attention ack <key> --disposition needed-human  # hide it until it clears
```

The `NEEDS` column is Jev's `needs_human` probability for the item, read from
`<town>/.runtime/jev/attention.jsonl` (see **Attention battery**); `gt attention
--help` states the order it gives the queue. The score clears nothing, because
its precision (43.8% at 98% recall over 228 labels, as `gt jev scorecard
--battery attention` reports it) is too low for that: every item stays listed
and a low score only sinks it. The dashboard's Attention panel draws the same
number beside each queue item and orders each alert level by it. Its risk-path
rows (post-landing reviews) are the exception: they are ranked among themselves
by the landing battery's risk answer, read from
`<town>/.runtime/jev/landing.jsonl` (see **Landing battery**).

A `-` in `NEEDS` is an item no answer names yet, and a log that was never
written leaves every item at `-`. A log that exists but cannot be read is a
different state: the table prints a `NEEDS unavailable` header and its cells
read `?`, so an unread answers log never looks like a queue Jev declined to
score. A failed read has one cause to look for — the file's mode, or a
directory where the log should be.

An ack must say what the item needed: `needed-human`, `fixed-by-overseer` or
`noise`. `gt attention ack` records it in `acks-history.jsonl`. `--disposition`
is required: a call without it, or with another value, exits 1 and acks
nothing, so a script that ran `gt attention ack <key>` must add the flag.

`--follow` prints `HH:MM:SS rule detail`, one line per event; `--json` prints
the `events.jsonl` line, and `--since DURATION` replays that window first. One
`--follow --wake-only` monitor replaces a monitor per condition.

The dashboard's Attention panel lists the wake events that stand: a `wake=true`
raised line is a row keyed by its `key` and tagged with its rule id until a
cleared line or an ack takes it off. The panel groups its rows into "Needs you"
and "In the pipeline" by `attention.Classify`, with each group's count in its
header: a pipeline row is work already in flight, and it names who holds it and
what it waits on. The strip's needs-you pill draws the human's rows from the
first group alone. Each new one is also announced as an alert, which is a toast
and a browser notification while the operator has alerts on. Where the
dashboard's own alert covers the same condition (red main, an open escalation, a
polecat stall, a stuck queue), only the attention alert fires.

The queue is four files under `<town>/.runtime/attention/`. The daemon keeps a
fifth there, `tips.json`: the origin tips the direct-push check has seen, so a
daemon restart does not drop a push nobody has acked. `gt attention ack` only
ever writes `acks.json` and `acks-history.jsonl`:

- `state.json` — the current set, rewritten atomically each daemon heartbeat.
  An item is `{key, kind, severity, rig, bead, sha, summary, first_seen,
  last_seen, acked_at}`; `key` is the condition's stable identity, so the same
  condition re-observed keeps its key and its `first_seen`.
- `events.jsonl` — the transitions, append-only, rotated to `events.jsonl.1` at
  1 MB. Each line is `{ts, class, severity, text, key, state, rule, handled,
  wake}`, `state` being `new` or `cleared`. The first four fields are the
  `alerts.jsonl` schema (`gt-z2pdg`) that `gt tail`'s watch source reads (see
  **Watch Feed**), so transitions show in the feed. `rule` is the stable id of
  the rule that raised the item (`landing.stuck_inflight`); `handled` is
  `self_healed`, `operator_action` or `info`; `wake` says the overseer or the
  human should be woken. A line without them reads as `info`, no wake. The one
  table of each rule's flags is `ruleFlags` in `internal/attention/rules.go`;
  `host.disk_warn` is `info` until it holds for three consecutive heartbeats,
  then the daemon appends one `operator_action` wake line for it.
  `landing.risk_path` is `info` until the landing is three hours old (the
  record's `landed_at`, held in the item) with no review note; the daemon then
  appends one `operator_action` wake line for it, once per item. A risk-path
  item's severity is review debt's — `low`, below the escalation, the red main
  and the daemon-down item beside it — unless the review that failed it was of a
  P1 landing, the lane the town's own machinery (daemon, guards, git, dolt)
  lands in; the one rule is `RiskPathSeverity` in
  `internal/attention/reviewdebt.go`.
- `acks.json` — the acknowledged keys, `{acks: [{key, at}]}`, written by `gt
  attention ack` under a flock. The daemon reads it and never writes it, so
  neither process races the other's file.
- `acks-history.jsonl` — one line per ack, `{ts, key, kind, rule, disposition}`,
  appended under the same flock and rotated to `acks-history.jsonl.1` at 1 MB.
  Unlike `acks.json` it outlives the item, so the attention battery's scorecard
  adapter can label a cleared item by what the human did; a line without a
  `disposition` predates the field and reads as `needed-human`.

The one table of each rule's owner, the human or the overseer, is `ruleOwners`
in `internal/attention/owner.go`.

An ack hides its item until the item's condition clears; the ack is then
dropped, so a recurrence raises a new item rather than hiding behind the old
ack. A state older than 15 minutes is stale — the daemon has stopped writing
it — and `gt attention` prints a `STALE` header and exits 3.

### Overseer Questions

A question the overseer has for Sloan is a bead in the town database under the
label `overseer-question`, so it survives the overseer's session and the
dashboard's Attention panel and the daemon read it.

```bash
gt ask "<title>" <<'EOF'   # file one; body sections below, on stdin
gt answer <id> "<text>"    # Sloan's answer: a comment with no prefix
gt answer --overseer <id> "<text>"   # the overseer's note: "[overseer] <text>"
gt questions [--json]      # the open ones as a priority queue, with held work
```

The body needs `## Question` and `## Recommended answer`; `## Held work` (bead
ids, `stub: <title>`, or `none`) and `## Facts` are optional. A held bead is made
to depend on the question, so it leaves `bd ready` until the question is closed.
Nothing closes a question automatically.

A question is a bead like any other, so `gt ask --priority 0-4` (default 2) files
one that can stop the town above one that can wait, and `--effort quick|session`
tags what answering takes as an `effort:` label. `gt questions` and the
dashboard's Questions payload order by priority and then by how long the question
has waited, and each row carries the priority, the count of beads depending on
the question and its effort tag.

A question is waiting on Sloan while its newest comment is the overseer's or
there is none, and answered once its newest comment lacks the `[overseer]`
prefix. The attention queue raises three rules for it (flags in `ruleFlags`):

- `question.new` wakes once for a question nobody has commented on.
- `question.answered` wakes for an open question whose newest comment is not
  the overseer's, and again every 20 minutes until the overseer comments.
- `question.stale` is info for a question open over 24 hours, repeated every 6
  hours.

The repeat intervals are `repeatEvery` in `internal/attention/repeat.go`. Each
question row on the dashboard shows the `gt answer <id> "..."` line Sloan runs;
the page stays read-only.

### Overseer Heartbeat and Journal

The overseer session reports to the town through two files under
`<town>/.runtime/overseer/`, so the dashboard and the daemon can tell whether it
is alive and what it keeps doing by hand. Neither command starts, stops or
restarts a session.

```bash
gt overseer beat [--state working|idle|away]   # heartbeat.json: {ts, state, session}
gt overseer log <kind> "<text>" [--bead ID]    # one line of journal.jsonl: {ts, kind, text, bead}
```

`--state working` is refused while any attention item is unacknowledged or any escalation is open
that no attention item already names (an `esc:` item is the same escalation). The refusal names each
unacked key, quoted, and the counts, and no heartbeat is written. `idle` and `away` are not refused.

`kind` is one of `file_bead`, `review`, `config_edit`, `hold`, `install`,
`nuke_seat`, `answer`, `decision`, `fix_by_hand`; any other kind is refused with
that list. The text is cut to 300 characters and collapsed to one line. The
journal is pruned the way the events log is (`events.Prune`, 90 days or 1 MB),
and the dashboard's Overseer panel counts its last seven days by kind: the
backlog for what to automate next.

The `overseer.silent` rule (info, no wake) holds while the heartbeat is older
than `overseer.silent_after` (default `90m`) and either the spec dispatcher's
newest tick, under ten minutes old, found ready beads, or an open question is
owed a move by the overseer: no comment yet, or answered by Sloan. A town whose
overseer has never beaten is silent. When `overseer.silent_push` names a
`notify.commands` entry, the daemon pushes through it once per silent period,
the period being one stale heartbeat; `silent-push.json` in the same directory
records the heartbeat time it pushed for. Both keys sit under `overseer` in the
operational settings:

```json
"overseer": {"silent_after": "90m", "silent_push": "ntfy"}
```

The rule acts on nothing else: it restarts, kills and unhooks no session.

### Idle Dispatcher Rule

`dispatch.idle` (operator_action, wakes) holds while the spec dispatcher's last
5 ticks each had a free seat in the roster, at least one dispatchable bead
waiting and nothing dispatched. A bead counts as waiting when its skip reason is
not one a free seat leaves unchanged: unshaped, not eligible (crew-only,
needs-human, overseer-only, operator-only, needs-overseer, needs-operator),
container, unknown formula, no seat of its class, rig hold, an unresolved
blocker and the per-tick limit do not count; a content-overlap hold
on a P2 or lower bead (P0 and P1 skip that hold, gt-iydx1) or an already-hooked
hold, a stale board read and a failed sling do. The summary names the three
commonest skip reasons with their bead counts, so a standoff over one overlap
reads as one line. The rule clears on a dispatch, when no dispatchable bead
remains, under an operator hold or when the newest tick is over 10 minutes old.
The thresholds are `attentionDispatchIdleTicks`, `attentionDispatchIdleFresh`
and `attentionDispatchIdleReasons` in
`internal/daemon/attention_dispatch_idle.go`.

### Review Backlog Rule

`review.backlog` (operator_action, wakes) is raised once when the number of
open `landing.risk_path` items passes `overseer.review_backlog_threshold`
(default `15`), or rises by more than `overseer.review_backlog_rise` (default
`10`) over the lowest count of the last hour. Its summary names the total and
the change, as `16 open risk-path reviews, +3 in the last hour (wakes past 15,
or a rise over 10)`. A risk-path item wakes on its own only after three hours
unreviewed, so a rule on the total is what catches a pile.

While it holds, more items change the summary and wake nothing. It clears,
without a wake, when the count falls under half the threshold, and the next
crossing raises it again. The count is the items the risk-path collector sees,
acked or not, because an ack hides a row and does not review the landing. The
one-hour history is kept in daemon memory, so a restart measures the rise from
its first tick. Both keys sit under `overseer` in the operational settings:

```json
"overseer": {"review_backlog_threshold": 15, "review_backlog_rise": 10}
```

A zero or negative value reads as the default.

### Watch Feed

Two files carry the town's own alerts in one schema, and `gt tail`'s watch
source reads both under the `watch` tag:

- `<town>/.runtime/watch/alerts.jsonl` — what the operator's monitor scripts
  write (`gt-watch`, outside this repo): a stuck in-flight landing, a stuck
  queue, a direct push, a stalled polecat, a slow `bd`, a health change.
- `<town>/.runtime/attention/events.jsonl` — the daemon's attention
  transitions (see **Attention Queue**); its first four fields are this
  schema, and `key` and `state` are extra, which a reader ignores.

One JSON object per line:

| field | meaning |
|-------|---------|
| `ts` | RFC3339, when the condition was seen |
| `class` | what kind of condition it is, e.g. `polecat-stall` |
| `severity` | `low` or `high` |
| `text` | the one-line description |

A writer appends and rotates at 1 MB — the attention log renames to
`events.jsonl.1` — and leaves a file it does not own alone. A line that is not
an object with a `ts` and a `low`/`high` severity is skipped, with one note in
the feed naming the file and the line's size, so a writer emitting one sees it.

### Hourly Report

`gt report --hour` prints the hourly summary the overseer used to assemble by
hand, from the town's own ledgers. `gt report --hour --help` is the section
list and each section's source. It only reads: no Dolt write, no mail, no
nudge. A source that cannot be read prints `<section>: unavailable
(<reason>)` in that section's place and the command still exits 0.

Two sections read writers that land later in the epic. `sweep` prints `no
sweep record` until the daemon's tier sweep has run (gt-vsct7.5). `attention`
prints unavailable until the daemon writes `.runtime/attention/state.json`
(gt-vsct7.2): a queue that was never written and an empty queue must not read
the same.

`spend` is the one section left out whole rather than reported empty — a
`.runtime/watch/spend.json` older than 15 minutes is not a live reading.
daemon.log is local time and bd JSON is UTC; the window is tz-aware, so a line
stamped just inside it counts in either zone.

### Jev Batteries

A Jev battery asks Jev one question per item and logs the answer. What every
battery shares lives in `internal/jev/battery`: the daily cap, the JSONL log
(`Append`, `ReadRecent`), the `{time, id}` cursor and the run lock.

The daily cap is one town-wide cap across every Jev caller. `Cap.Spent` reads
the usage ledger (`.runtime/jev/usage.jsonl`) and sums everything stamped since
local midnight whatever the line's `caller`, so a second battery's calls today
use up the first one's room. `Cap.Allows` is whether today's spend is still
under `MaxUSDPerDay`. The ledger stores tokens, so `USDPerBillionTokens` is
required to price them.

### Collection health (`gt jev health`)

`gt jev health` is the one command that answers whether Jev collected today: it
judges every battery from the files each one writes, the patrol config and
`usage.jsonl`. The statuses, thresholds and row fields are in `gt jev health
--help`, and `--json` prints the same rows. The exit status is 1 when an
enabled battery is not collecting, so a cron or a morning check reads it
without parsing the table.

The rules live in `internal/jev/health` as pure functions over what the command
read, so each status is a unit test over fixtures. Two readings are worth
knowing: a battery only a landing writes is judged against the landings the
town recorded, never against a clock, and a log line that no longer parses is
counted in its row, so a schema drift reads as unparsed lines rather than as a
quiet day.

### Attention battery

`internal/jev/attention` asks one Jev call per raise: each `new` transition in
the attention queue's event log (`.runtime/attention/events.jsonl`) past the
cursor, whether or not the item still stands. Each call asks four questions
(needs a person, clears on its own, repeats an open item, and an urgency score),
and the pass appends one line per raise to
`.runtime/jev/attention.jsonl`. The line's `live` says whether `state.json` still
held the item when the pass asked. A raise that cleared first is asked with
`live: false`, no rig, bead or SHA, and no other open items beside it. The pass
is advisory and log-only: it never acks, hides, wakes or changes an item, and it
writes nothing under `.runtime/attention`.

The attention battery replaced the three-way Jev triage of the attention queue
and its replay (`gt attention jev-replay`, gt-3ey5j, now retired). Use
`gt jev attention` for the answers and `gt jev scorecard --battery attention`
to score them.

Keys live under `patrols.jev_attention` in `daemon.json`. `enabled` defaults to
false, `max_usd_per_day` to 1, and `usd_per_billion_tokens` is required. The
daily cap is the town-wide one described under Jev Batteries: it counts every
Jev caller's spend, so other batteries' calls today use up this pass's room.
The daemon runs a pass every 15 minutes when enabled. The first pass only sets
the cursor, so it asks about no raise before it. The pass reads only the current
`events.jsonl`: a cursor older than that file's first line logs one warning and
the pass continues from the file's start. `gt jev attention --recent N` prints
the last N lines.

A failed call is held until a later call in the same pass succeeds. Then the
failure was about the item, so the item is logged with `answers: null` and not
asked again. If three calls in a row fail the pass stops as an outage: those
items are not logged, the cursor stays before them, and the next pass asks them
again. The pass summary and the daemon log count answered items, null-answer
lines and held calls apart, so a pass where Jev was down does not read like one
where it answered.

### Landing battery (`jev_landing`)

Before running or changing the pass, read `gt jev landing --help`: it states
what the five questions cover, that the first pass only starts the cursor, and
what `--recent` does. The pass reads the landings in
`.runtime/landings/gastown.jsonl` past its cursor and appends one line per
landing to `.runtime/jev/landing.jsonl`.

The log line is `{ts, bead, rig, head, landed_at, agent, answers,
question_version, model}`. `answers` maps each question id to its answer:
`needs_review_now`, `touches_town_machinery`, `beyond_goal` and
`tests_cover_change` each hold a `noul` probability, and `risk` holds a `score`
(0 docs or text only, 1 one package with tests, 2 across packages or without
tests, 3 town machinery or what other agents read) with its `confidence`.
`question_version` is `landing.Version` and changes whenever a question or a
rubric level does; `model` is the model ID the API returned. The log holds ids,
numbers and answers, never prompt or diff text.

The state Jev reads is the bead's title and `## Goal` section (cut to 1200
runes), `risk_paths`, `om_verdict`, `om_score`, `gate_result`, `agent`, and the
changed files from `git diff --numstat base..head` as paths with added and
removed counts (first 60 files). Each value is JSON-quoted. No diff content is
sent.

A landing whose bead or diff cannot be read, or that Jev returns an unusable
response for, is skipped with no line and counted as skipped in the daemon log;
any other failure stops the pass, leaves the cursor, and is retried next pass.

The daemon patrol `jev_landing` runs the pass every 15 minutes, off the
heartbeat. It is off unless `patrols.jev_landing.enabled` is true in
`daemon.json`.

### Submission battery (`jev_submission`)

`gt jev submission` asks Jev about each gastown work bead submitted for landing.
The pass reads the first-sight records the landing worker wrote (see below), past
the cursor by seen time, and each waiting bead with a READY TO LAND block that no
record names at its head. The records are what keep a fast landing in the log:
the landing worker removes the label and prunes the branch when it lands the
bead. It asks ten questions in one call and appends one line per submission to
`.runtime/jev/submission.jsonl`. The
pass is advisory and log-only: it adds no bead comment, no attention item and
nothing under `.runtime/landings`, and it never holds up a landing. Read the
log with `gt jev submission --recent N`.

The log line is `{ts, bead, branch, head, target, submitted, agent, answers,
question_version, model}`. `answers` maps each question id to its answer:
`tests_cover_change`, `beyond_goal`, `commits_match_change`,
`touches_town_machinery`, `claims_verified` and `verification_applies` each hold
a `noul` probability, and `risk` holds a `score` (the landing battery's four
levels) with its `confidence`. Lines at `question_version` 2 or lower also carry
`will_pass_review`, `boxes_credible` and `outcome`, a `choice` over `complete`,
`partial`, `blocked` and `unclear` with its `probabilities` and `positive`
(`complete`); the battery stopped asking them at version 3. `question_version`
is `submission.Version` and changes whenever a question or a rubric level does;
`model` is the model ID the API returned. `agent` is the bead's `agent:` label,
else its `agent=` attached var. The log holds ids, times, numbers and answers,
never prompt, title or diff text.

The state Jev reads is the bead's title, its `## Goal`, `## Acceptance` and MR
notes. The sections are each cut to 1500 runes. The MR notes are the bead's
notes from its last READY TO LAND block on, cut to 4000 runes, so an earlier
attempt's rejection stays out. The state also carries the commit subjects on
`target..head` (at most 20) and the changed paths from `git diff --numstat
target..head` with added and removed counts (at most 80 files). Each value is
JSON-quoted. No diff
content is sent.

A submission whose record holds its commits is asked from them, and the pass
never touches the clone for it. Any other submission, a record with no subjects
or a waiting bead with no record, reads its commits and stat from a bare clone
the pass owns at `.runtime/jev/repos/gastown.git`. The pass creates the clone
from the rig registry's `git_url` the first time it needs one and fetches
`refs/heads/*` into it before each such read. The pass never runs git in the
rig's `mayor/rig` checkout or any town worktree. A fetch that fails stops the
pass and leaves the cursor unmoved.

The first pass sets the cursor to its own time and asks nothing. A submission
the clone cannot read, or that Jev returns an unusable response for, is skipped
with no line and counted as skipped; any other failure stops the pass, leaves
the cursor, and is retried next pass.

The landing worker writes `.runtime/jev/submissions-seen.jsonl`, one line per
submission the first time it sees a bead's READY TO LAND block at a head:
one `land.SubmissionRecord` as JSON.
`subjects` (at most 20, newest first) and `numstat` (at most 80 lines) are read
from the rig's own clone, `.repo.git`, which the polecat worktrees share, so the
write makes no fetch and no network call. A clone that lacks the commits writes
the line with no subjects and logs the read error, and the battery reads its own
clone for it. A stat that cannot be read drops the subjects too, so a line holds
both or neither. The write
happens before the worker lands the bead, so a landing that prunes the branch
still leaves the submission for the battery to ask about. A write that fails is
logged once per run of failures and never holds up a landing. The file is
append-only, 0600 in a 0700 directory. The first run after the log is added
starts it empty, so waiting beads with no record are asked from the label until
records appear.

The daemon patrol `jev_submission` runs the pass every 15 minutes, off the
heartbeat. It is off unless `patrols.jev_submission.enabled` is true in
`daemon.json`. The cap and pricing keys are in `gt jev submission --help`; the
command reads them whether or not the patrol is on.

### Drift battery (`jev_drift`)

`internal/jev/drift` asks Jev whether each landing left the docs behind. A pass
takes every landing since its cursor, risk path or not, and for each one reads
the doc sections closest to the landing's diff. It asks Jev once per section and
appends one line per landing to `.runtime/jev/drift.jsonl`. The pass is advisory
and log-only: it edits no doc, files no bead, adds no attention item and never
holds up a landing.

The pass reads the diff with `git diff --numstat` and a zero-context `git diff`
over `base..head`, leaving out Markdown and `_test.go` files. It reads the docs
at the landing's target branch, `refs/heads/<target>`, from the bare clone the
submission battery owns at `.runtime/jev/repos/gastown.git`. It never reads
`mayor/rig` or a town worktree. The pass syncs that clone once, before its first
landing, and a failed sync stops the pass with the cursor unmoved.

Sections are chosen in one of two ways. When `hm` is on the PATH, the pass runs
`hm context --diff - --json` and takes the doc paths its hits name, and it
searches those docs first. When hm is missing, its reply names no doc with a
matching section, or the call fails, the pass greps every `docs/**/*.md` for the
diff's identifiers: flags such as `--recent`, snake_case or kebab-case names such
as `jev_drift`, and `gt <command>` phrases. Only the changed lines count. Each
section is cut to 1500 runes, and at most five sections are asked per landing.

Each section gets one Jev call with three questions. `doc_now_stale` and
`needs_new_section` are `noul` probabilities. `drift_kind` is a `choice` over
`command`, `flag`, `config_key`, `behavior`, `none` and `unclear`. The state
holds the changed paths with counts, up to 3000 runes of diff hunks, and the
section's path, heading and text, each value JSON-quoted.

Each line is `{ts, bead, rig, head, landed_at, agent, sections, unanswered,
question_version, model}`. `sections` holds one entry per section that got an
answer, each `{doc, heading, answers}`. `unanswered` counts the sections Jev
gave an unusable response for, and the pass does not ask them again. The log
holds paths, headings, numbers and answers, never diff or doc text. A landing
whose diff names no docs-relevant change gets a line with an empty `sections`
list.

A git read that fails for any reason other than a refused ref stops the pass
with the cursor on that landing, so the next pass reads it again. A landing
whose refs or doc paths the pass refuses before git runs, or whose every section
returns an unusable response, is skipped with no line. A spend cap reached
mid-landing stops the pass without writing that landing, so the next pass asks
it again in full.

The daemon patrol `jev_drift` runs the pass every 15 minutes, off the heartbeat.
It is off unless `daemon.json` enables it, and it reads the same three keys as
`jev_submission` above, under `jev_drift`:

```json
"patrols": {
  "jev_drift": {
    "enabled": true,
    "max_usd_per_day": 1.0,
    "usd_per_billion_tokens": 3.0
  }
}
```

### Daemon-log triage battery (`jev_logtriage`)

`internal/jev/logtriage` asks Jev about the town daemon's warning and error
lines. Every 15 minutes a pass reads `daemon/daemon.log` from its cursor and
keeps the lines containing `warning`, `error`, `failed`, `refused`, `timeout`,
`crash` or `panic`, in any case. Two rules narrow that. A keyword preceded by a
count keeps the line only when the count is above zero, so `0 refused`,
`0 failed` and `0 error(s)` do not match. `warning` keeps the line only at the
start of its message, as `WARNING:`, or after a count above zero such as
`2 warning(s)`. A count may carry thousands separators, so `1,000 refused`
matches. It groups the kept lines by pattern key:
the line with its times, bead ids, hashes, durations and numbers replaced by
placeholders. Each group records the keyword that kept its first line as
`matched_on`.

Each group gets one Jev call with five questions. `is_routine_noise`,
`needs_action` and `auto_remediable` are `noul` probabilities. `severity` is a
`score` over `info`, `watch`, `review` and `page`. `category` is a `choice` over
ten parts of the town. The state holds the group's key, count, first and last
time, and up to three sample lines, each JSON-quoted.

Each group appends one line to `.runtime/jev/logtriage.jsonl`:
`{ts, key, count, first, last, samples, matched_on, answers, question_version,
model, reason}`. The samples are the daemon's own lines, so the triage log is as
sensitive as the daemon log it came from. The battery is log-only: it raises no
attention item, suppresses no line and pages no one.

A pass asks about the 50 largest groups, ties by key. The rest are counted as
dropped and their lines are not read again. The cursor is a byte offset plus the
file's inode, so a rotated log starts again at its first line. A last line still
missing its newline waits for the next pass. With no cursor yet, the first pass
reads from the start of the log. A call that gets no usable reply logs a reason
and no answers, and its lines are not asked again. If the day's cap runs out
partway through a pass, the groups it did not reach are dropped. A pass that
starts with the cap spent reads nothing.

Rotation is detected by inode on Unix only. Elsewhere, a rotated log is caught
only when it shrinks below the cursor's offset.

The daemon patrol `jev_logtriage` is off unless `daemon.json` enables it. It
reads the same keys as `jev_drift`, under `jev_logtriage`:

```json
"patrols": {
  "jev_logtriage": {
    "enabled": true,
    "max_usd_per_day": 1.0,
    "usd_per_billion_tokens": 3.0
  }
}
```

`gt jev scorecard --battery logtriage` scores the log in `internal/jev/scorecard/logtriage.go`.
Each line is one group, one item on the five questions. The labels come from what
the town raised in the hour after the group's last daemon line, and the rule that
sets them is named in `--json`:

- an attention event in `events.jsonl` that is not `self_healed`, or an escalation
  bead, raised in that hour, whose text has the group's `category` word or a bead
  id from its samples as a whole word: `needs_action` true and `is_routine_noise` false
  (`logtriage:follow-up-within-hour`). An escalation's text is its title.
- an escalation of that kind also labels `severity`: true (a page) when the
  escalation is `critical` or `high`, false (a review) when it is `medium`, `low`
  or unset. The scorecard judges a score by the mean score of each label.
- a `self_healed` attention event that shares the subject: `auto_remediable` true
  (`logtriage:self-healed`). It does not set `needs_action` on its own.
- no such follow-up: `needs_action` false and `is_routine_noise` true
  (`logtriage:no-follow-up-within-hour`). `is_routine_noise` mirrors `needs_action`, so
  the two scores are one piece of evidence. It is set only when the window has closed
  and every matching record's time was readable.
- no `self_healed` event matched: `auto_remediable` false
  (`logtriage:no-self-healed-follow-up`), unless a matching follow-up's time could not
  be read, which leaves it unlabeled.
- a group whose last line is less than an hour old is logged but unlabeled, since
  its window is still open. So is a group with no timestamp, and a group with no
  `category` and no bead id in its samples. A cleared event never counts.
- a matching event or escalation whose time cannot be read leaves the group
  unlabeled, unless a readable follow-up in the hour sets the label.

The scorecard reads the events and the escalation beads once per run. It opens no
Jev call.

### Test-selection battery (`jev_testsel`)

The landing gate runs every Go package. The test-selection battery asks Jev which
packages a landing's diff cannot affect, and logs that beside what the gate found.
It is shadow only: the gate runs everything, as before. The battery writes its log
line and the Jev usage ledger for its one call, and nothing else (gt-bp9qd).

It runs when the landing worker starts a rig's candidate gate, not on a ticker.
The pass starts before the gate and is finished after the gate's verdict, so Jev
answers while CI runs. A failure at any step logs a line with a `reason` and
never fails the landing. The pass reads the merged tree in the landing's work
clone: `git diff` from the merged commit's first parent, and `go list ./...`.

The code decides first, and Jev is asked about the rest:

- **always run**: a changed package, a package whose code or tests directly import
  a changed package, or every package when `go.mod`, `go.sum`, `Makefile`, `.forgejo/`,
  `.github/` or any `testdata/` changed (`reason` `tree_wide`, nothing asked).
- **asked**: every other package, in one call, one `noul` per package: could this
  change affect behavior that the package's tests verify?

A package that reaches a changed package only through another package is asked.
Those are the packages a direct-import rule would skip, so they are the ones the
battery grades.

The state holds the diff stat, the changed files with their counts, the changed
packages, and each package with its in-module imports, its test files' imports
included. It never holds diff text. It is capped at about 20k tokens; a larger
tree logs `too_large` and asks nothing. The Jev call is bounded at two minutes; a
call that runs past that logs `ask_failed`, so a hung call never holds a landing.

The log is `.runtime/jev/testsel.jsonl`, one line per landing:

```json
{"bead":"gt-x","rig":"gastown","head":"…","changed":["internal/land"],
 "always_run":["internal/land","internal/daemon"],"asked":["internal/cmd"],
 "noul":{"internal/cmd":0.12},
 "would_skip":{"0.2":["internal/cmd"],"0.3":["internal/cmd"],"0.5":["internal/cmd"]},
 "miss":{"0.2":[],"0.3":[],"0.5":[]},
 "ungraded":{"0.2":[],"0.3":[],"0.5":[]},
 "gate_packages":212,"gate":{"internal/cmd":true},
 "gate_seconds":{"internal/cmd":31.4},"question_version":1,"model":"…"}
```

`would_skip` holds the asked packages whose `noul` is below the threshold. `miss`
holds the would-skip packages the gate reported as `FAIL`. `ungraded` holds the
would-skip packages the gate reported no result for. Those are neither a miss nor
a pass: nothing was graded. `gate`
is absent when the candidate's CI job log had no package lines to read.
`gate_seconds` holds each package the gate timed, so the scorecard can price a
skip; a package the gate reported `(cached)` is absent. `reason` is
set when Jev was not asked: `too_large`, `tree_wide`, `nothing_to_ask`,
`cap_unreadable` (the spend ledger could not be read, so the cap is unknown),
`cap_reached`, `unreadable_tree` or `ask_failed`.

`patrols.jev_testsel` in `daemon.json`:

```json
"patrols": {
  "jev_testsel": {
    "enabled": true,
    "max_usd_per_day": 1.0,
    "usd_per_billion_tokens": 3.0
  }
}
```

| Key | Meaning |
|-----|---------|
| `enabled` | Turns the battery on. Default `false`. |
| `max_usd_per_day` | The town-wide daily cap every Jev battery shares. `0` takes the default of 1. |
| `usd_per_billion_tokens` | Prices the ledger's tokens. Required: without it the battery logs an error and the gate runs unwrapped. |

`gt jev scorecard --battery testsel` scores the log in `internal/jev/scorecard/testsel.go`.
A line answers each package Jev was asked about, so each asked package is one item
on the `affects_tests` question (`noul`: could the change affect the package's tests).
The gate's result for that package labels it:

- the gate reported the package `FAIL`: affected, `affects_tests` true (`testsel:gate-result`).
- the gate reported it `ok`: not affected, `affects_tests` false (`testsel:gate-result`).
- the gate reported no result for the package, or the line's `gate` is absent:
  logged but unlabeled. Nothing was graded.

The report prints one summary line per threshold, 0.2, 0.3 and 0.5, beside the
per-question block: the asked packages the battery would skip at it (`noul` below
the threshold), how many of those were ungraded, how many the gate failed (MISS),
and the gate minutes those packages took (`gate_seconds` summed; when only some
were timed, the figure sums just those and is followed by `(N of M timed)`, e.g.
`1.5 (1 of 3 timed)`). A threshold whose would-skip packages were all untimed, or
that has none, prints `n/a`.

### Review-targeting battery (`jev_targets`)

`gt jev review-targets --run <bead> --ranked <rows> --history <file>` asks Jev
about each file in a deep-review rotation slice and appends one line per file
to `.runtime/jev/targets.jsonl`. It reads the ranked slice rows
`scripts/deep-review.sh` writes (tier, last review date, risk score, bytes,
path, highest risk first) and the history that ranked them
(`git log --name-only --format=@%s`, which tells each file its recent commit
subjects). At most 60 files are asked about, in rank order. The battery is
advisory and log-only: it never reorders, drops or holds up the slice, and the
log is the only thing it writes.

The log line is `{ts, run, path, tier, risk_score, bytes, model, answers,
question_version}`. `run` is the deep-review run bead whose slice held the
file, so the scorecard labels the answers from that run's findings. `answers`
maps each question id to its answer: `likely_defect`, `logic_churn` and
`under_tested` each hold a `noul` probability, and `review_priority` holds a
`score` (0 skip this run, 1 review when the slice has room, 2 review first)
with its `confidence`. `question_version` is `targets.Version`.

The state Jev reads is the file's path, tier, last review date, risk score,
line count, its last ten commit subjects and its exported function, method and
type names (at most 40). Each value is JSON-quoted. The body is read from the
repository at the ranked ref (`--repo`, `--ref`) for the line count and the
names, and never sent.

`scripts/deep-review.sh plan --mode slice` calls the command once, after it
builds the slice, when `patrols.jev_targets.enabled` is true. It passes
`slice-rows.txt` and `history.txt`, sends the command's output to stderr and
ignores its exit status, so the slice is the same whether the battery runs,
fails or is off. The run stops for the day once the town's Jev spend reaches
`patrols.jev_targets.max_usd_per_day` (default 1) and needs
`patrols.jev_targets.usd_per_billion_tokens` to price that spend. The
command reads the keys whether or not the battery is enabled, so a manual run
works before it is switched on. They live under `patrols.jev_targets` (the
`daemon` section of `settings/config.json`, the legacy `daemon.json` on an
unmigrated town):

```json
"patrols": {
  "jev_targets": {
    "enabled": true,
    "max_usd_per_day": 1.0,
    "usd_per_billion_tokens": 3.0
  }
}
```

| Key | Meaning |
|-----|---------|
| `enabled` | Turns the battery on. Default `false`. |
| `max_usd_per_day` | The town-wide daily cap the run checks with `Cap.Allows` before every call. `0` takes the default of 1. |
| `usd_per_billion_tokens` | Prices the ledger's tokens. Required: a run without it refuses to run. |

### Scorecard (`gt jev scorecard`)

`gt jev scorecard --battery <name>` scores a battery's log against the labels
its adapter finds, one block per question. Batteries only log; the scorecard is
where their answers are judged. Flags are in `gt jev scorecard --help`. The
command is read-only. A battery with no log yet gives an empty report with
`log_missing` set (`No log yet` in the text), and exits 0.

Each battery is an adapter in `internal/jev/scorecard`, registered by name from
an `init` with `scorecard.Register(name, factory)`; the factory takes the town
root. The adapter implements:

- `Battery()`: the name passed to `--battery`.
- `LogPath()`: the JSONL log the battery appends to.
- `Parse(raw) (Item, error)`: one log line as an `Item` (id, time, model, the
  answers keyed by question id, and any adapter refs it needs to label). An
  error skips the line and counts it in `lines_skipped`.
- `ParseLines(raw) ([]Item, error)`, for an adapter whose line answers more than
  one item. The scorecard reads such a line through it in place of `Parse`; a
  drift line answers each doc section it names.
- `Label(ctx, item) (Labeling, error)`: the item's labels, whether it is labeled
  at all, the rule that set them, and their source when an adapter draws labels
  from more than one. An unlabeled item counts as logged but not
  labeled. A label on an item with no usable answer to the question counts as
  `unmatched` and is not scored. An error that cannot be read fails the whole
  report, so a broken label source cannot pass as "no labels yet".

An adapter may also implement `Context() []string`, which adds context lines to
the report: facts gathered while labeling that the metrics do not carry. They
print under the item counts, and the JSON report carries them as `context`.

An item may also carry `Context`: a value per question, printed beside that
question's block and counted over its logged lines, never scored or labeled.

Each question is scored by the kind of answer Jev gave. A noul is scored at
0.5. A choice names its `positive` option, and precision and recall are at the
option it chose, with the threshold sweep over that option's probability. A
score has no cut at 0.5; it reports the mean score by label and the best cut.

The verdict for each question:

- `not-yet`: fewer than 30 labeled lines.
- `earning`: on at least 30 labeled lines, the best F1 beats the label base
  rate (positives over labeled lines) by 0.1 or more.
- `no-signal`: anything else.

The best F1 is taken over every distinct value as a cut, predicting yes at or
above it; a tie keeps the higher cut. `--json` names the rule that labeled each
line in its `lines` array, and the label's source where it has one.

Every battery is registered with `Register` in `internal/jev/scorecard/`. The
test-only `fixture` battery reads `.runtime/jev/fixture.jsonl` with labels from
`fixture-labels.json`, and exists so tests can drive the command end to end. No
live battery reads it.

The `dispatch` battery reads `.runtime/jev/dispatch.jsonl`, and its labels are
described under the dispatch battery below.

The `attention` battery reads `.runtime/jev/attention.jsonl`, the attention
battery's log. A line whose call failed, or that was asked under a question set
newer than the scorecard reads, is skipped; an older set is read, and its
answers to the questions the battery still asks are scored. Labels come from
the attention queue's retained files (`.runtime/attention`: events, acks and the
acks history) and the beads each item names. An item's first matching rule
labels it; the rules in the order they apply, the labels each sets and the rule
id `--json` reports are in `internal/jev/scorecard/attention.go`. A rule that
labels `routine_action_fixes` is not counted on a line that gave no answer for
it: the battery dropped the question at set 2.

An item still standing with no disposition is unlabeled, as is one whose life
the queue files no longer hold. A bead read that fails for any reason other
than not-found fails the report.

The `landing` battery reads `.runtime/jev/landing.jsonl`. A landing's label
comes from a review of its logged head, and every label records its source,
`overseer` or `polecat`; the report counts each question's labels by source. The
overseer's note on the bead wins: the latest `OVERSEER REVIEW <head> PASS|FAIL`
line for the logged head, read with the parser the risk-path attention item
uses. When the notes hold no such verdict — none, or a `WAIVED` or `AUDITED`
line — the label comes from the bead's polecat review document,
`docs/reviews/<date>-<bead>.md` on the bare clone's `main`, whose verdict line
is `POLECAT REVIEW <head> PASS|FAIL[: <the defect>]`; the head may be
abbreviated to at least 7 hex digits, as a review document usually names it. A
clone that cannot be read fails the report, as a missing rig does: otherwise
every landing it would have labeled would read as unreviewed with a clean exit.
A `PASS` from either source sets `needs_review_now` and `risk` false
(`landing:review-pass`), a `FAIL` sets both true (`landing:review-fail`), and a
head neither review records leaves the item unlabeled. The finding beads a
`FAIL` names in the bead's notes are counted in the report's context.

The `submission` battery reads `.runtime/jev/submission.jsonl`, the submission
battery's log. Each line is joined to the om review that judged it: the first
row in `.runtime/landings/reviews.jsonl` for the same bead, at or after the
logged `submitted` time and before the bead's next submission. An `approve` row
sets `will_pass_review` true (`submission:om-approve`), a `request_changes` row
sets it false (`submission:om-request-changes`), and a line with no row, or a
row with any other verdict, is unlabeled.

The joined row's attempt names a `MERGE REJECTION` block in the bead's notes,
and that block's findings set two more labels, by their titles, matched
case-insensitively at a word start and through its inflection (`no test` matches
`no testing` and `no tests`, but not the `no test` inside `piano test`):

- a title that holds a missing-test phrase sets `tests_cover_change` false;
- a title that holds a scope-beyond-the-bead phrase sets `beyond_goal` true.

The two phrase lists are `missingTestPhrases` and `beyondGoalPhrases` in
`internal/jev/scorecard/submission.go`, and nowhere else. A question no title
names stays unlabeled on that line. A `request_changes` row whose attempt has no
block in the notes adds `submission:no-rejection-block` to the line's rule, so
a review whose findings could not be read differs from one that had none. The
bead's notes are read once per report, however many submissions it has.

That block's kind also sets labels. A `gate` refusal sets `boxes_credible` false
(`submission:gate-rejected`). A `review` refusal whose findings include a major
or critical sets `claims_verified` false (`submission:om-major`). A `conflict`
refusal sets none of the four new questions.

An approval the first attempt landed sets `claims_verified`, `boxes_credible`
and `outcome` true (`submission:first-try-landing`). That needs four things:
the joined row's verdict is `approve`, the row is the first attempt, the bead's
`LANDING RECORD` names the submission's head, and no rejection block sits on
that attempt. The outcome
label is true for `complete`, the choice's positive option. A line that
matches none of these leaves the four questions unlabeled.

A line's `rule` names every rule that set a label, space-separated. The report's
context counts the reviews that labeled submissions and the findings those
reviews carried.

The `targets` battery reads `.runtime/jev/targets.jsonl`. Its labels come from
the deep-review run bead each line names. Before changing a ledger line shape,
read the ledger comment at the top of `scripts/deep-review.sh`: it defines the
`reviewed:`, `finding:`, `queued:` and `p3:` lines, and the adapter parses them
by hand.

A closed run labels the files its ledger names: a `finding:`, `queued:` or
`p3:` line sets `likely_defect` and `review_priority` true (`targets:ledger`),
and a `reviewed:` line with no such line sets both false (`targets:clean`). The
file a line names is the first token of its fingerprint; a line whose fields do
not parse is skipped, as the script skips it. A file the ledger does not name
stays unlabeled, because a missing or unreadable ledger is not evidence that the
file is clean. A run bead still open, or no longer in the store, labels nothing.
Each run adds a context line naming the files it scored, the files with
findings, the files the ledger does not name, and where its first finding ranks
by Jev's `review_priority` beside its rank in the slice's risk order.

The `finding` battery is described next.

##### Finding adapter (`--battery finding`)

`internal/jev/scorecard/finding.go` scores the log `gt jev dedupe` writes
(`.runtime/jev/dedupe.jsonl`). Each line is labeled from its finding bead, read
through the beads store, by the bead's close reason (case-insensitive). A bead
still open, or no longer in the store, leaves the line logged but unlabeled.
`--json` names the rule as the close reason's class: `close:landed`,
`close:no-changes`, `close:superseded-duplicate-wontfix` or, when none of those
applies, `close:names-candidate`.

| Close reason | Labels |
|--------------|--------|
| starts with `landed` | `fix_worthy` true, `report_only` false, `any_duplicate` false |
| starts with `no-changes` | `fix_worthy` false, `report_only` true |
| contains a not-fix-worthy word (the list is `notFixWorthyWords` in `internal/jev/scorecard/finding.go`) | `fix_worthy` false |
| names a candidate the line offered, as a whole bead id | `any_duplicate` true |
| any other reason | no label |

`any_duplicate` is false only for a `landed` or `no-changes` reason that names
no candidate and has no duplicate wording. A reason that says duplicate or
superseded without naming an offered candidate leaves it unlabeled, because the
duplicate may be one the search did not offer.

The `duplicate` Choice is labeled whenever `any_duplicate` is. It is correct
when it chose a candidate the reason names, or `none` when the reason names no
candidate. The adapter reads the Choice's chosen option as its positive option
and its confidence as the probability, so the question's precision is how often
the chosen option was right and its calibration bands read confidence against
that rate.

The `severity` Score is labeled only for a `landed` reason that carries a
`landed_commit:` (or `commit_sha:`) line. The label is the commit's lines added
plus removed against its first parent: under 50 is false, over 300 is true, and
50 to 300 is unlabeled. A commit the rig repo no longer holds leaves it
unlabeled; a repo that cannot be read fails the report. `test_only` has no
label.

#### Dispatch battery (`gt jev dispatch`)

`internal/jev/dispatch` predicts, for each bead just placed on a seat, whether
it lands on the first merge attempt, fits twenty minutes, has tickable
acceptance, needs a reasoning model and touches the daemon, and scores its size
on a three-level rubric. It reads beads after the fact in its own pass and is
never called from the spec dispatcher's tick or from a sling, so dispatch
latency is unchanged. It is advisory and log-only: it never edits, labels or
comments on a bead.

A placement is a bead in `in_progress` or `hooked`, or one closed since the
last pass, whose description carries an `attached_at` stamp later than the
pass's cursor (`.runtime/jev/dispatch.cursor.json`, the stamp with the bead id
as tie-break). The closed read keeps a placement that finishes between two
passes in the battery; without it the log would sample only the slow ones. A
bead slung again gets a new stamp and a new line. The first pass only starts
the cursor.

The state quotes the title, the `## Goal`, `## Constraints`, `## Acceptance` and
`## Size` sections (each cut to 1500 runes), the labels and the agent (the
`agent:` label, else the `agent=` entry in `attached_vars`), each as a JSON
string. Raise `Version` in `questions.go` with any change to a question; every
line carries it.

Each pass appends one line per bead to `.runtime/jev/dispatch.jsonl`:
`{ts, bead, attached_at, agent, answers, question_version, model}`. `answers`
holds a probability for each noul question and `size: {score, confidence}`;
lines at `question_version` 1 also hold `lands_first_try`. `model` is the
versioned model the API answered with, so a model upgrade shows in a scorecard.
`gt jev dispatch --recent N` prints the last N lines.

A question that fails logs no line, and its cause never reaches the log. The
placement goes into `.runtime/jev/dispatch.retries.json` with an attempt count
and is asked again on the next pass, so a single transient failure costs time,
not the prediction. A placement that fails three times is given up, and the
daemon log names the bead and the cause.

A failure that means Jev was out of reach — a timeout, a lost connection, a 401,
a 5xx, a 429 that outlived its retry — is not the bead's fault. The pass stops
at that placement and reports it, spending no attempt and dropping nothing, so
an outage of any length leaves every pending bead for the next pass. Only a
request Jev refused on its own terms (a 4xx other than 401) or answered
unusably counts against a bead.

`gt jev scorecard --battery dispatch` scores that log. Each line is one
placement, stamped with its `attached_at`. The noul questions are scored as noul
and `size` as a score. Labels come from three sources, and a question with no
rule for a placement is unlabeled and left out of its counts:

- `lands_first_try`: the bead's first attempt in
  `.runtime/landings/reviews.jsonl`. Only lines at `question_version` 1 carry
  its answer; the battery stopped asking it at version 2. approve is true
  unless the landing refused it (`rejected`), and request_changes is false.
  With no review, a bead closed as landed is true.
- `fits_twenty_minutes`: the bead's `.runtime/overrun-guard/<bead>.json`. A
  record with `stopped_at` is false. A bead closed as superseded, or labeled
  `needs-split`, is false. A record still pending leaves it unlabeled. With
  neither and a landed bead, it is true.
- The bead itself, from the rig's beads: a bead closed with a landing close
  reason is landed.

The `--json` report names the rules that labeled each line, joined with `+`:
`review:approve`, `review:refused`, `review:request-changes`,
`landed:no-review`, `overrun:stopped`, `closed:superseded`, `label:needs-split`
and `landed`.

The seat's agent prints beside `needs_strong_model` as context. The adapter only
reads the rig's beads and never writes. A source that cannot be read fails the
report, and a bead the store no longer has is unlabeled. Lines are not split by
`question_version`: a line at version 1 is scored as a current one, with its
`lands_first_try` answer.

The daemon patrol `jev_dispatch` (heartbeat step `jev-dispatch`) is off unless
`daemon.json` sets it on, and runs every 15 minutes:

```json
{"patrols": {"jev_dispatch": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required; `max_usd_per_day` defaults to 1 and is
the town-wide cap above, so the dedupe and dispatch batteries draw on one day's
room. `gt jev dispatch` reads the same keys whether or not the patrol is on.

### Conflict battery (`jev_conflict`)

The daemon patrol `jev_conflict` (heartbeat step `jev-conflict`) is off unless
`daemon.json` sets it on, and runs every 15 minutes:

```json
{"patrols": {"jev_conflict": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

For each bead placed since the last pass, the pass pairs it with the beads that
were in flight when it was placed, at most five, and asks Jev about each pair
once. The answers are `same_files` and `same_function`, both `noul`, and `risk`,
a `score` from 0 (no shared package) to 3 (the same function). Each pair appends
one line to `.runtime/jev/conflict.jsonl`. The pass changes no bead and no
dispatch decision, and it shares the town-wide cap with the other Jev batteries.

`gt jev scorecard --battery conflict` labels a pair positive when one bead's
conflict rejection was built on a target that held the other's landing, or when
the two landings changed a file in common. It labels a pair negative when both
landed and changed no file in common, and neither was rejected for a conflict
it cannot attribute to the other bead. The rest are unlabeled: a rejection the
clone cannot trace, such as one built on a tip it does not hold, settles
nothing.

### Progress battery (`jev_progress`)

When the overrun guard nudges a polecat at its nudge threshold, the nudge is
queued. The progress pass runs on the next heartbeat that finds a nudge queued,
so the answer lands while the seat is still inside its clock. The pass reads the
seat's transcript for the window before the nudge and asks five questions in one
call: `making_progress`, `looping`, `blocked_on_environment` and
`drifted_from_bead`, each a `noul` probability, and `remaining`, a `score` whose
levels run from `barely started` up to `done`, so a finished bead scores at the
top. It appends one line to
`.runtime/jev/progress.jsonl`: `{ts, rig, polecat, bead, session, nudge_at,
model, answers, question_version}`. A level's position is its score, so the
scorecard reads one question set only: a line logged under another one is
skipped rather than swept with its direction reversed.

A nudge that cannot be asked logs null `answers` and a `reason` instead. The
reasons are `no_transcript`, `transcript_unreadable` (missing or unreadable),
`outside_town` (the transcript is not under an allowed root, and is never opened),
`bead_unreadable`, `bad_response` (Jev returned nothing usable) and `ask_failed`.
The pass goes on to the next nudge after each. A line that cannot be appended
to the log ends the pass: the nudges it did not reach are dropped, and the
daemon logs the pass as stopped with its counts.

The battery is log-only. It never nudges, stops, messages or comments on a seat,
and the guard's decisions never read the log. The state Jev reads is the bead's
title and acceptance, then the window's events: assistant text in full, each tool
call's input and each tool result cut to a fixed length, and every value passed
through the report's credential scrub. The state has a token budget, estimated
from its JSON size; when it is over, the oldest events go first and
`events_dropped` counts them. The pass reads only the end of a transcript. When
the window reaches back past what it read, the state carries
`window_clipped: true`, so the events from its start are known to be missing.

A transcript is opened only after its symlinks are resolved and the real path is
under an allowed root. The allowed roots are the town root and the Claude projects
directories the daemon searches for the nudged seat's worktree, under the config
dir and under `~/.claude` (`projects/<hash>` for that worktree). Another seat's
projects directory is not allowed, and nothing above the projects directories is.

The queue lives in the daemon's memory and is bounded. A nudge is not held for a
later pass: a pass that finds the town's daily cap spent, or cannot open, drops
the nudges it has, and logs how many.

The daemon patrol `jev_progress` is off unless `daemon.json` enables it. While it
is off the guard queues nothing:

```json
{"patrols": {"jev_progress": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required. `max_usd_per_day` is optional and draws on
the same town-wide cap as the other batteries.

`gt jev scorecard --battery progress` scores that log. Each line is one nudge,
stamped with its `nudge_at`, and a line younger than 30 minutes is unlabeled.
The labels come from the bead's `LANDING RECORD` note and the overrun guard's
`.runtime/overrun-guard/<bead>.json`:

- `making_progress` true, and `remaining` true (done or nearly done), when the
  bead's landing record falls within 20 minutes after the nudge
  (`progress:landed-in-window`).
- `remaining` false when no landing falls in that window: the bead was not shown
  done or nearly done at the nudge (`progress:not-landed-in-window`). The label
  reads the nudge's window, not the bead's end state, so a bead that lands later
  is still false for the earlier nudge. A bead the store no longer holds takes
  no remaining label, since its state cannot be read.
- `making_progress` false when the overrun guard stopped the seat at or after
  the nudge (`progress:stopped-by-overrun`). A landing in the window wins over a
  stop.
- `blocked_on_environment` true when the overrun record charged gate time seen
  at or after the nudge (`progress:gate-wait`). Network waits are not recorded,
  so no rule labels them.

`looping` and `drifted_from_bead` are not labeled: no record names a restart or
a repeated command, and the guard's stop keeps no reason text. The rules are in
`internal/jev/scorecard/progress.go`.

### Stale-bead battery (`jev_stale`)

The daemon patrol `jev_stale` (heartbeat step `jev-stale`) runs once a week over
every operational rig. It takes each open bead untouched for 14 days, oldest
first, up to 500 beads a run, and asks Jev one call per bead: the noul
`obsolete`, `superseded`, `worth_doing` and `needs_reshaping`, and the score
`priority_fit` over `much lower`, `lower`, `right` and `higher`. It appends one
line per bead to `.runtime/jev/stale.jsonl`: `{ts, rig, bead, type, priority,
updated_at, desc_hash, landed_offered, model, answers, question_version}`, with a
`reason` and null `answers` when the bead could not be asked. `desc_hash` is the
SHA-256, in hex, of the bead's description when it was asked about: the log never
holds the description's text. A line logged before the field existed has none.

`landed_offered` lists the beads the question could weigh against `superseded`:
the ten closest, in `hm query` rank order, among beads closed as `landed` in the
last 30 days. With no `hm` on the PATH, or a failed search, it lists the ten
most recent landed beads in the rig.

A bead is asked at most once in 30 days; the pass reads that from the log, so a
second pass inside the window asks nothing. A bead whose call got no reply
(`ask_failed`) does not count, so the next pass asks it again.

The battery reads work beads only: type `task`, `bug` or `feature`. It never
asks about rig, molecule, agent, role, epic or wisp beads, any other type
(including the ones the beads package marks infrastructure), a crew seat's bead
(an ID containing `-crew-<name>`), or a bead labeled `human-only`, `overseer-only`,
`operator-only`, `handoff`, `needs-human`, `needs-overseer`, `needs-operator`,
`gt:needs-human`, `gt:needs-overseer` or `gt:needs-operator` (`human-only` and
`needs-human` still hold until they are retired). A rig whose beads cannot be
read is skipped and counted in the daemon log.

The battery is log-only: it never changes a bead's status, labels, priority or
comments. The E-stop holds the patrol and stops a running pass between Jev calls;
a pass the E-stop stopped does not count as the week's run, so the next pass
runs once the E-stop clears.

The patrol is off unless `daemon.json` enables it:

```json
{"patrols": {"jev_stale": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required. `max_usd_per_day` is optional and draws on
the same town-wide cap as the other batteries.

`gt jev scorecard --battery stale` scores that log against each bead as it
stands at scorecard time. A line whose bead no longer exists is unlabeled. A
line whose `type` is not `task`, `bug` or `feature` (an earlier pass asked
about rig and molecule beads) is discarded and counted as a skipped line. The
rules are in `internal/jev/scorecard/stale.go`:

- closed as `wontfix`, `obsolete` or `no longer ...`: `obsolete` true
  (`stale:closed-obsolete`).
- closed as `superseded ...` or `duplicate ...`: `superseded` true
  (`stale:closed-superseded`).
- landed, either the close reason `landed` or a landing record's `Landed on ...`:
  `worth_doing` true, and `obsolete` and `superseded` false (`stale:landed`).
  When the description's hash no longer matches `desc_hash`, `needs_reshaping`
  is also true (`stale:reshaped`). A line with no `desc_hash`, and a bead whose
  description is empty now, are never labeled `needs_reshaping`: neither shows
  text to compare. A bead asked about with no description and given one since
  is labeled, because its `desc_hash` is the hash of the empty text.
- priority changed since the ask: `priority_fit` true when it was raised (a
  lower P number), false when it was lowered (`stale:priority-raised`,
  `stale:priority-lowered`). The log does not record who changed it, so any
  change counts.

A bead still open with its priority unchanged is unlabeled. Its logged answers
still appear in the report, with no labels.

`gt jev scorecard --battery drift` scores the drift log against the docs' history
on `refs/heads/main`, read from the bare clone the submission battery owns. A
drift line answers each doc section it names, so each section is one item. The
scorecard does not fetch the clone: the drift pass's sync keeps it current. The
rules are in `internal/jev/scorecard/drift.go`. The window is 30 days after the
ask, from just after the ask's instant through the 30th day:

- a commit on the docs branch within the window touches the section:
  `doc_now_stale` true (`drift:docs-edit`). The section's lines are those from
  its heading to the next heading in the doc before the commit. A hunk whose old
  lines fall in them touches it, and so does a hunk that adds the heading line.
- a bead closed as landed within the window, with a close reason `landed` or
  `Landed on ...`, whose title or description names the doc file as a whole path
  (`old/docs/a.md` does not name `docs/a.md`): `doc_now_stale` true
  (`drift:docs-bead-landed`). The rule names the file, not the section. A rig
  with no beads directory, or a closed bead whose close time does not parse,
  fails the report rather than labeling the section.
- no such commit or bead, and the window has passed: `doc_now_stale` false
  (`drift:no-edit-30d`). Inside the window with neither, the section is unlabeled.
- `drift_kind` is labeled only when a commit touched the section. It is true when
  the kind Jev picked is one the touching hunks name (`drift:kind-in-edit`). A
  `--flag` names a flag, a `gt <command>` phrase a command, and a snake_case or
  kebab-case name a config key. A hunk that names none is a `behavior`. `none`
  and `unclear` are never right for an edit. The probability scored is the one
  Jev gave the option it picked.
- `needs_new_section` is never labeled.

### Notes battery (`jev_notes`)

The daemon patrol `jev_notes` (heartbeat step `jev-notes`) runs once a week over
the operator's memory notes and handbook: the `*.md` files in
`~/.claude/projects/<town root with non-alphanumerics as "-">/memory/` and
`~/.claude/commands/gastown.md`, read-only. Per file it asks Jev one call: the
noul `still_describes_the_town` and `names_something_removed`, and the choice
`kind` over `path`, `command`, `flag`, `config_key`, `process`, `none` and
`unclear`. The state holds the file name, its modified date, today's date and
the text, capped at 12000 characters. It appends one line per file to
`.runtime/jev/notes.jsonl`: `{ts, source, file, hash, label, checked, missing,
unresolved, model, answers, question_version}`, with a `reason` and null
`answers` when the file could not be asked. `hash` is the SHA-256 of the file;
the log never holds its text.

A file is asked again only when its hash changes or 30 days pass, read from the
log's lines. A file whose call got no reply (`ask_failed`) does not count. A pass
asks at most 400 files and draws on the town-wide daily cap. The patrol edits and
deletes no note. The E-stop holds it, and stops a running pass between calls.

The pass also writes the mechanical label, with no Jev call. It takes the
backticked tokens of the file (inline spans, and the command lines of fenced
blocks) that look like:

- a repo path: relative, two or more segments, under a top-level directory of the
  rig's `refs/heads/main` tree or one of `internal`, `cmd`, `docs`, `pkg`,
  `scripts`, `templates`, `formulas`; a path under a dot directory is never
  checked. The tree is read from the bare clone the submission battery owns. A
  `:line` suffix is dropped. A note about another repository that uses one of
  those directory names reads as `stale`.
- a `gt`, `bd` or `hm` invocation: its leading subcommand words and its `--flags`,
  checked against the installed binary's `--help`. A word after a command whose
  usage takes arguments is an argument, not a subcommand. A binary that is not
  installed, or that prints one usage for every `--help` (`hm`), is checked only
  at its root.
- a config key: snake_case, or dotted snake_case, found quoted in a Go source on
  `main`.

`label` is `stale` when a path is missing from the tree or a subcommand or flag
is absent from `--help` (`missing` lists them), `current` when at least one token
was checked and all resolved, and empty otherwise. A config key found nowhere is
listed in `unresolved`; it keeps the file from `current` but does not make it
`stale`.

The patrol is off unless `daemon.json` enables it:

```json
{"patrols": {"jev_notes": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required.

`gt jev scorecard --battery notes` scores the log against the `label` on each
line, with no further reads. The rules are in `internal/jev/scorecard/notes.go`:

- `stale`: `still_describes_the_town` false and `names_something_removed` true
  (`notes:stale-token`). `kind` is true when the option Jev picked is the kind of
  a missing token (`path`, `command` or `flag`).
- `current`: `still_describes_the_town` true and `names_something_removed` false
  (`notes:all-tokens-resolve`). `kind` is true only when Jev picked `none`.
- no label: the file is logged and unlabeled.

### Failing-test cause battery (`jev_failcause`)

When a landing's candidate gate goes red on a Go test, the battery reads the
gate's failure excerpt (the same excerpt the rework note carries, cut to 3000
characters) and asks Jev once per failing test: the noul `will_recur`, whether
the test will fail again on another landing within 14 days, and the choice
`cause` over `shared_state`, `timing_assumption`, `host_environment`,
`test_bug`, `product_regression` and `unclear`. It appends one line per failing
test to `.runtime/jev/failcause.jsonl`: `{ts, rig, bead, package, test, answers,
model, question_version}`. The excerpt is never logged. A Jev call that gets no
usable reply writes no line, so the next red verdict is judged afresh.

The battery is shadow only. It wraps the rig's candidate gate, returns the
gate's result untouched, and judges in the background, so a landing never waits
on Jev. Nothing is retried, skipped or excused, and no answer feeds back into
the landing. The patrol is off unless `daemon.json` enables it:

```json
{"patrols": {"jev_failcause": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required. `max_usd_per_day` is optional and draws on
the same town-wide cap as the other batteries.

`gt jev scorecard --battery failcause` labels `will_recur` from
`.runtime/ci-failures.jsonl`, the ledger the landing worker writes, in the
scorecard's `internal/jev/scorecard/failcause.go`:

- the same test failed on a different bead within 14 days of the judging:
  `will_recur` true (`failcause:recurred-within-14d`). This label does not wait
  for the window to close.
- the window has closed with no such failure, and the ledger's last write came
  strictly before the judging plus the ledger's 7-day retention, the shortest
  it has ever kept: `will_recur` false (`failcause:quiet-14d`). A later write
  prunes records older than the retention it was written under, and the ledger
  kept 7 days before it kept the full window, so a longer gap can hide a
  repeat. A write recorded exactly at
  the boundary stays unlabeled too: that time is the file's mtime, which the
  filesystem can floor, not the clock the daemon pruned by.
- otherwise unlabeled: the window is open, or the ledger is missing.

`cause` has no mechanical label. It is logged for a person to read.

### Refused-review cause battery (`jev_rework`)

When a landing's om review refuses the work, the battery reads the refusal's
findings from the work bead's MERGE REJECTION block for that attempt (cut to
3000 characters) and the files the refused head changed against its target, and
asks Jev one question: the choice `cause` over `tests_missing`, `scope_creep`,
`logic_error`, `wrong_file`, `docs_stale`, `convention` and `unclear`. The noul
`reworked_to_land` was dropped at question set 2 (gt-xu56u); the scorecard still
labels it from the landings file for the older lines that carry its answer. It
appends one line per refusal to `.runtime/jev/rework.jsonl`: `{ts, refused, rig,
bead, attempt, head, verdict, score, answers, model, question_version}`.
`refused` is when the landing logged the review, which is where the scorecard's
label starts. The findings are never logged. A refusal whose bead or rejection
block cannot be read is skipped for good, and so is one whose Jev call gets no
usable reply. Any other Jev failure stops the pass, so the next one asks again.

A refusal is a review the landing refused the work on: `request_changes`, or an
approval whose findings blocked it. A pass reads refusals in order and moves its
cursor past each one before the next, so a refusal is asked about once. The
pass reads the logs, the beads and the rig's repository, and changes no landing.
The patrol is off unless `daemon.json` enables it:

```json
{"patrols": {"jev_rework": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required. `max_usd_per_day` is optional and draws on
the same town-wide cap as the other batteries.

`gt jev scorecard --battery rework` labels `reworked_to_land` from the rig's
landings file, in the scorecard's `internal/jev/scorecard/rework.go`, by the
first landing of the bead after the refusal's own time. A line at question set
2 or later carries no answer for it, so it is left unlabeled:

- that landing is the refused head: `reworked_to_land` false, since the work
  landed without a rework commit (`rework:same-head-landed`).
- that landing is another head: `reworked_to_land` true, since the bead landed
  only after a later submission (`rework:reworked-before-landing`).
- the bead has not landed since the refusal, or the rejection block named no
  head: unlabeled.

`cause` has no mechanical label. It is logged so a person can read which kinds
of work fail most.

### Thrash battery (`jev_thrash`)

Each bead the spec dispatcher places on a seat is asked about once per placement
(a bead placed twice earns a line for each), including a bead that closed before
the pass ran. The state is the bead's title, its Goal, Constraints, Acceptance and
Size sections and its agent, each cut to a length cap. Jev answers two noul
questions: `will_respawn`, whether the polecat will be respawned at least once on
this bead, and `needs_split`, whether the overrun guard will stop the polecat for
running long. A pass appends one line per placement to
`.runtime/jev/thrash.jsonl`: `{ts, bead, attached_at, agent, answers, model,
question_version}`. The line holds bead ids, the agent name and numbers, never
the bead's text. The pass edits, labels, holds and splits no bead. A bead whose
own request Jev refuses is skipped for good; any other Jev failure stops the
pass, so the next one asks again. The patrol is off unless `daemon.json` enables
it:

```json
{"patrols": {"jev_thrash": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required; the daily cap is the town-wide one
described under Jev Batteries.

`gt jev scorecard --battery thrash` labels each line from two files under
`.runtime/`, in the scorecard's `internal/jev/scorecard/thrash.go`:

- `will_respawn` from `bead-respawn-counts.json`: a count below 2 is a first run,
  false (`respawn-count`). A respawn stamped after the placement is true. A later
  placement of the same bead settles an earlier one as false; the newest
  placement stays unlabeled until a respawn follows it.
- `needs_split` from `overrun-guard/<bead>.json`: a record with `stopped_at` is
  true (`overrun-stop`), a record with no stop stamp is false
  (`overrun-no-stop`), and a stop that has begun but not finished is left
  unlabeled (`overrun-stop-pending`).

A missing file labels nothing. A file that exists but cannot be read leaves its
question unlabeled, and the report says so.

### Standing-rules battery (`jev_rules`)

After `gt done` submits a bead for landing, the battery reads the last 4000
characters of the submitting session's transcript and asks Jev two questions:
the noul `broke_standing_rule`, whether the session broke a standing rule (ran
the full gate itself, pushed to main, wrote outside its own worktree, or printed
a secret), and the choice `rule_kind` over `gate_run`, `main_push`,
`outside_tree`, `secret`, `none` and `unclear`. It appends one line per
submission to `.runtime/jev/rules.jsonl`: `{ts, rig, bead, polecat, done_at,
excerpt_sha256, model, answers, question_version, reason}`. The line holds ids,
times, the excerpt's SHA-256 and the answers, never transcript text. A
submission that could not be asked gets a line with null `answers` and a
`reason`: `no_transcript`, `transcript_unreadable`, `other_session` (the seat's
newest transcript began after the submission), `bad_response` or
`request_refused`. A transcript the containment check refuses to open gets no
line. The pass blocks, punishes and labels no seat or bead. It is off unless
`daemon.json` enables it:

```json
{"patrols": {"jev_rules": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required; the daily cap is the town-wide one
described under Jev Batteries.

`gt jev scorecard --battery rules` labels `broke_standing_rule` in the
scorecard's `internal/jev/scorecard/rules.go`. A line younger than 20 minutes is
unlabeled. Otherwise a guard record for the bead inside the 20 minutes after the
submission labels it true: a revert-guard refusal in the refusal ledger
(`rules:revert-guard`) or a rework stand-down comment on the bead
(`rules:rework-stand-down`). With no record, and every source readable, it is
false (`rules:no-guard-record`); a bead the store does not hold leaves it
unlabeled. `rule_kind` has no mechanical label, since no guard record names a
kind.

## Beads Commands (bd)

Read with `bd`:

```bash
bd ready                     # Work with no blockers
bd list --status=open
bd list --status=in_progress
bd show <id>
```

Write with `gt bead`:

```bash
gt bead create "..." --type=task   # File a bead
gt bead claim <id>                 # in_progress, assigned to you
gt bead close <id>
gt bead dep add <child> <parent>   # child depends on parent
```

### Reading a bead's history

`bd history <id>` prints one snapshot per retained Dolt commit. A flatten
(the offline procedure in [dolt-history-offline.md](dolt-history-offline.md), or
the routine flattening the compactor-dog plugin did before it was removed)
discards those commits, and the command does not say so — a bead whose
snapshots begin at the flatten reads exactly like one whose snapshots begin at
its creation, so a field showing a single value looks like a field that never
changed.

```bash
gt history <id>              # Where the snapshots start, and whether that is truncated
gt history <id> --json       # Machine-readable verdict
bd history <id> --events     # Change events, which a flatten preserves
```

Check the floor before concluding that a field never changed before it, and
read `--events` for the span the commit snapshots lost.

## Formula Invocation Patterns

Workflow formulas (sequential steps, single polecat) run one of two ways:

Examples: `shiny`, `shiny-enterprise`, `mol-polecat-work`

```bash
# Sling it: gt cooks the formula and pours a wisp for the target
gt sling <formula> --on <bead-id> <target>
gt sling shiny-enterprise --on gt-abc123 gastown

# Or dispatch it from gt: ready steps become beads, wired by their needs
gt formula run <formula> --rig=<rig>
```

`gt formula run` dispatches workflow formulas only. A formula of another type
is reported and nothing is dispatched; the convoy formula type and its
formulas (code-review, design, mol-plan-review, mol-prd-review) were deleted
(gt-gzhin.5).

```bash
gt formula show <name>   # Shows the formula's type and steps
bd formula list          # Lists formulas
```

## Common Issues

| Problem | Solution |
|---------|----------|
| Agent in wrong directory | Check cwd, `gt doctor` |
| Beads prefix mismatch | Check `bd show` vs rig config |
| Worktree conflicts | Check worktree state, `gt doctor` |
| Stuck worker | `gt nudge`, then `gt peek` |
| Dirty git state | Commit or discard, then `gt handoff` |

> For architecture details (bare repo pattern, beads as control plane, nondeterministic idempotence), see [architecture.md](design/architecture.md).
