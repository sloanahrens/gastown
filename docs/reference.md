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

**Landing and gate fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `setup_command` | `string` | `""` | Setup/install command (e.g., `pnpm install`) |
| `typecheck_command` | `string` | `""` | Type check command (e.g., `tsc --noEmit`) |
| `lint_command` | `string` | `""` | Lint command (e.g., `eslint .`) |
| `test_command` | `string` | `""` | Test command to run. Empty = skip. `gt done`'s default test-verify gate inherits any leading `VAR=value` assignments from it, and writes the container opt-in off for the run unless the command turns `GT_TEST_DOCKER=1` on itself (gt-wx53) — see below. |
| `presubmit_command` | `string` | `""` | What `gt done` runs on the rebased branch before it pushes. Empty = `make presubmit` when a Go repo's Makefile has that target (lint, build, and the tests of the changed packages only), else `make gate`. The rig's Forgejo CI gate runs the full `make gate` on the candidate branch (gt-fn9e6.32). A command that turns `GT_TEST_DOCKER=1` on is refused, as for `test_command`. |
| `build_command` | `string` | `""` | Build command (e.g., `go build ./...`) |
| `post_land_command` | `string` | `""` | Run once per landing at the landed commit, and at any new tip of the default branch that no landing put there (a direct push). Red-main reads the units a red run blamed out of the run's own output: its failing Go packages, and the scripts `scripts/tier-sweep.sh`'s red summary line named. A command that runs the shell tests without that sweep names no failing script, so only a Go failure is attributable there. A red run whose sole landing since the last green commit can have moved what failed is reverted through the landing worker and its bead reopened for rework. A landing that cannot have moved it leaves the red-main bead to a human: a failing script needs one of the shell tier's inputs in the landing's diff, a failing Go package a Go input (gt-40so9). State in `.runtime/red-main/<rig>.status` and `<rig>.json`; the spec dispatcher reads the revert a rig has in flight out of `<rig>.json` and holds that rig's red-main beads until it lands (gt-zkdwt). Read from `settings/config.json` only. Empty disables it. |
| `max_ready_for_dispatch` | `int` | `0` | `gt sling` refuses new work while the rig has more ready MRs than this. 0 = guard off. |
| `followup_backpressure` | `int` | `40` | Open `om-followup` beads at which the follow-up feed (`landing.followups`) stops filing for this rig. The bound is counted as beads are filed, so a landing that splits into one bead per package stops at it rather than carrying the count past it. The count falling back under the bound resumes the feed on the next landing, and while it holds the feed back it logs how many of the landing's findings were left without a bead and raises one low attention item a day (gt-4ty22). Unset means 40. |
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
| `followups` | `bool` | `false` | File shaped `om-followup` beads, priority 3, for the minor findings of a landing om approved at or above the rig's `.om.json` threshold, whatever it scored. A passing review carries no major or critical finding for the feed to file — either refuses the landing back to its author (gt-oi4je) — so its minors are all there is. The findings go in one bead, or one bead per package when they reach more than one package or number more than six. A finding matching an existing follow-up by fingerprint, or by the same path with a similar title, comments `seen again on <bead>` there instead, and the work bead gets one comment naming the beads filed. A landing whose own bead wears `om-followup` files nothing, so a fix's minors never feed the feed again. A retry of a landing attempt that filed before failing refiles none of what it filed, and files the packages that attempt never reached — it says nothing to its own beads, since `seen again` is for a finding another landing raised. `merge_queue.followup_backpressure` open `om-followup` beads stop it. Every read, comment and file is fail-soft and bounded at 20s: the landing is recorded first, and the feed never changes its result. |

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

**Re-queueing a rejected landing (gt-3e1z4).** A landing the worker refuses labels the
bead `rework` (removing `gt:ready-to-land`) and appends a `MERGE REJECTION` note naming
the head it refused. A bead the human-only label holds keeps its assignee: its branch
comes from a person, so the refusal goes back to the crew that submitted it rather than
onto the board (gt-me800). When the polecat's branch still sits on that head the
rejection came from something other than the diff, and nothing needs to change.
`gt land requeue <bead> --reason "<why>"` removes `rework`, restores `gt:ready-to-land`
and records the operator, the reason and the head as a bead comment, so the worker gates
and merges the same head again. It refuses when the head changed (a normal `gt done`
resubmission, which re-gates), when the bead is not in a rejected state, and when the
branch is gone from the remote; it never re-queues a head that has not passed presubmit.
`gt done` on a rework bead still on the rejected head does not resubmit: it stands down,
leaves the bead open, and tells the polecat to ask for a `gt land requeue` if the
rejection was environmental, or to make a new commit otherwise.

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
an `agent:<name>` label.
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
bd update gt-rig-myrig --add-label=polecat_branch_template:adam/{year}/{month}/{description}
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
| `CLAUDE_RUNTIME_CONFIG_DIR` | Custom Claude settings directory |

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
| **Crew** | `~/gt/<rig>/crew/<name>/rig/` | Persistent human workspace clone |
| **Polecat** | `~/gt/<rig>/polecats/<name>/rig/` | Polecat worktree (ephemeral sandbox) |

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
bd dep add gt-def gt-abc                 # gt-def waits for gt-abc to land
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
gt handoff --shutdown        # Terminate (polecats)
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
gt attention                 # the queue, oldest first
gt attention --all           # include acked items
gt attention --json          # state.json
gt attention -f              # stream new transitions as they append
gt attention --follow --wake-only   # only the events that need a decision
gt attention ack <key> --disposition needed-human  # hide it until it clears
```

An ack must say what the item needed: `needed-human`, `fixed-by-overseer` or
`noise`. `gt attention ack` records it in `acks-history.jsonl`. `--disposition`
is required: a call without it, or with another value, exits 1 and acks
nothing, so a script that ran `gt attention ack <key>` must add the flag.

`--follow` prints `HH:MM:SS rule detail`, one line per event; `--json` prints
the `events.jsonl` line, and `--since DURATION` replays that window first. One
`--follow --wake-only` monitor replaces a monitor per condition.

The dashboard's Attention panel lists the wake events that stand: a `wake=true`
raised line is a row keyed by its `key` and tagged with its rule id until a
cleared line or an ack takes it off. Each new one is also announced as an alert,
which is a toast and a browser notification while the operator has alerts on.
Where the dashboard's own alert covers the same condition (red main, an open
escalation, a polecat stall, a stuck queue), only the attention alert fires.

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
  Unlike `acks.json` it outlives the item, so the Jev attention replay can
  label a cleared item by what the human did; a line without a `disposition`
  predates the field and reads as `needed-human`.

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
needs-human), container, unknown formula, no seat of its class, rig hold and the
per-tick limit do not count; a content-overlap or already-hooked hold, a stale
board read and a failed sling do. The summary names the three commonest skip
reasons with their bead counts, so a standoff over one overlap reads as one
line. The rule clears on a dispatch, when no dispatchable bead remains, under an
operator hold or when the newest tick is over 10 minutes old. The thresholds are
`attentionDispatchIdleTicks`, `attentionDispatchIdleFresh` and
`attentionDispatchIdleReasons` in `internal/daemon/attention_dispatch_idle.go`.

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

### Attention battery

`internal/jev/attention` asks one Jev call per new attention item: five
questions (needs a person, clears on its own, a routine action fixes it, repeats
an open item, and an urgency score), and appends one line per item to
`.runtime/jev/attention.jsonl`. The pass is advisory and log-only: it never
acks, hides, wakes or changes an item, and it writes nothing under
`.runtime/attention`.

Keys live under `patrols.jev_attention` in `daemon.json`. `enabled` defaults to
false, `max_usd_per_day` to 1, and `usd_per_billion_tokens` is required. The
daily cap is the town-wide one described under Jev Batteries: it counts every
Jev caller's spend, so other batteries' calls today use up this pass's room.
The daemon runs a pass every 15 minutes when enabled. The first pass only sets
the cursor, so it asks about no item raised before it. `gt jev attention
--recent N` prints the last N lines.

A failed call is held until a later call in the same pass succeeds. Then the
failure was about the item, so the item is logged with `answers: null` and not
asked again. If three calls in a row fail the pass stops as an outage: those
items are not logged, the cursor stays before them, and the next pass asks them
again. The pass summary and the daemon log count answered items, null-answer
lines and held calls apart, so a pass where Jev was down does not read like one
where it answered.

### Landing battery (`jev_landing`)

`gt jev landing` asks Jev a second opinion on each gastown landing whose record
has `risk_paths`. It reads `.runtime/landings/gastown.jsonl` past its cursor,
asks five questions about each landing in one call, and appends one line to
`.runtime/jev/landing.jsonl`. The line is advisory and log-only: it adds no bead
comment and no attention item. Read it with `gt jev landing --recent N`.

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

The first pass sets the cursor to its own time and asks nothing. A landing whose
bead or diff cannot be read, or that Jev returns an unusable response for, is
skipped with no line and counted as skipped in the daemon log; any other
failure stops the pass, leaves the cursor, and is retried next pass.

The daemon patrol `jev_landing` runs the pass every 15 minutes, off the
heartbeat. It is off unless `daemon.json` enables it, and `gt jev landing` reads
the same keys whether or not the patrol is on:

```json
"patrols": {
  "jev_landing": {
    "enabled": true,
    "max_usd_per_day": 1.0,
    "usd_per_billion_tokens": 3.0
  }
}
```

| Key | Meaning |
|-----|---------|
| `enabled` | Turns the patrol on. Default `false`. |
| `max_usd_per_day` | The town-wide daily cap the pass checks with `Cap.Allows` before every call. `0` takes the default of 1. |
| `usd_per_billion_tokens` | Prices the ledger's tokens. Required: a pass without it refuses to run. |

### Scorecard (`gt jev scorecard`)

`gt jev scorecard --battery <name>` scores a battery's log against the labels
its adapter finds, one block per question. Batteries only log; the scorecard is
where their answers are judged. Flags are in `gt jev scorecard --help`. The
command is read-only.

Each battery is an adapter in `internal/jev/scorecard`, registered by name from
an `init` with `scorecard.Register(name, factory)`; the factory takes the town
root. The adapter implements:

- `Battery()`: the name passed to `--battery`.
- `LogPath()`: the JSONL log the battery appends to.
- `Parse(raw) (Item, error)`: one log line as an `Item` (id, time, model, and
  the answers keyed by question id). An error skips the line and counts it in
  `lines_skipped`.
- `Label(ctx, item) (Labeling, error)`: the item's labels, whether it is labeled
  at all, and the rule that set them. An unlabeled item counts as logged but not
  labeled. An error that cannot be read fails the whole report, so a broken
  label source cannot pass as "no labels yet".

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
line in its `lines` array.

Two batteries are registered. The `fixture` battery reads
`.runtime/jev/fixture.jsonl` with labels from `fixture-labels.json`, and exists
so tests can drive the command end to end. No live battery reads it.

The `attention` battery reads `.runtime/jev/attention.jsonl`, the attention
battery's log. A line whose call failed, or whose question set is not the one
the scorecard reads, is skipped. Labels come from the attention queue's retained
files (`.runtime/attention`: events, acks and the acks history) and the beads
each item names. An item's labels come from the first rule that applies:

1. **A disposition** a human gave the item in `acks-history.jsonl` while it
   stood. It labels the item even while the item still stands, and it wins over
   the rules below. `needed-human` sets `needs_human` true (`ack:needed-human`);
   `noise` sets `needs_human` false and `clears_on_its_own` true (`ack:noise`);
   `fixed-by-overseer` sets `routine_action_fixes` true
   (`ack:fixed-by-overseer`). A line with no disposition does not count here.
2. **The kind's clearing mechanism**, for an item that has cleared:
   - `escalation`: its bead was closed with a close reason. A close by the
     person or the overseer (the `closed_by` address is the human's) sets
     `needs_human` true (`escalation:closed-by-person`); a close by a producer
     sets `clears_on_its_own` true (`escalation:cleared-by-producer`).
   - `risk-path`: the latest `OVERSEER REVIEW <sha>` line on its bead. `FAIL`
     sets `needs_human` true (`risk-path:review-fail`); `PASS` sets it false
     (`risk-path:review-pass`). `WAIVED` and `AUDITED` label nothing.
   - `rejected-twice`: a bead closed as `superseded` sets `needs_human` true
     (`rejected-twice:superseded`). A bead closed by a landing sets
     `routine_action_fixes` true when the last landing record names a branch
     that is not a `crew/` branch (`rejected-twice:landed-routine`).
3. **The self-clear window** (`cleared-within-15m`): an item that cleared within
   15 minutes of being raised, with no ack, no note on its bead and no close
   reason, sets `clears_on_its_own` true. An item whose bead no longer exists
   counts as having no note.

An item still standing with no disposition is unlabeled, as is one whose life
the queue files no longer hold. A bead read that fails for any reason other
than not-found fails the report.

#### Dispatch battery (`gt jev dispatch`)

`internal/jev/dispatch` predicts, for each bead just placed on a seat, whether
it lands on the first merge attempt, fits twenty minutes, has tickable
acceptance, needs a reasoning model and touches the daemon, and scores its size
on a three-level rubric. It reads beads after the fact in its own pass and is
never called from the spec dispatcher's tick or from a sling, so dispatch
latency is unchanged. It is advisory and log-only: it never edits, labels or
comments on a bead.

A placement is a bead in `in_progress` or `hooked` whose description carries an
`attached_at` stamp later than the pass's cursor (`.runtime/jev/dispatch.cursor.json`,
the stamp with the bead id as tie-break). A bead slung again gets a new stamp
and a new line. The first pass only starts the cursor.

The state quotes the title, the `## Goal`, `## Constraints`, `## Acceptance` and
`## Size` sections (each cut to 1500 runes), the labels and the agent (the
`agent:` label, else the `agent=` entry in `attached_vars`), each as a JSON
string. Raise `Version` in `questions.go` with any change to a question; every
line carries it.

Each pass appends one line per bead to `.runtime/jev/dispatch.jsonl`:
`{ts, bead, attached_at, agent, answers, question_version, model}`. `answers`
holds the five probabilities and `size: {score, confidence}`; `model` is the
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

The daemon patrol `jev_dispatch` (heartbeat step `jev-dispatch`) is off unless
`daemon.json` sets it on, and runs every 15 minutes:

```json
{"patrols": {"jev_dispatch": {"enabled": true, "max_usd_per_day": 1, "usd_per_billion_tokens": 4}}}
```

`usd_per_billion_tokens` is required; `max_usd_per_day` defaults to 1 and is
the town-wide cap above, so the dedupe and dispatch batteries draw on one day's
room. `gt jev dispatch` reads the same keys whether or not the patrol is on.

## Beads Commands (bd)

```bash
bd ready                     # Work with no blockers
bd list --status=open
bd list --status=in_progress
bd show <id>
bd create --title="..." --type=task
bd update <id> --status=in_progress
bd close <id>
bd dep add <child> <parent>  # child depends on parent
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
