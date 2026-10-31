# Power runbook

`gt power` manages the town's three power states. Before stopping the daemon,
the watchers or the Forgejo stack, read this page: the orderings and the drain
below are what keep a shutdown from killing work.

Run it from inside the town (`cd ~/gt`) or with `GT_TOWN_ROOT` set, the way
every `gt` command finds its town. An operator shim that wraps `gt power` from
anywhere does that `cd` itself.

## The states

| State | Parts | Dispatch |
|---|---|---|
| `full` | Dolt, the Forgejo stack and the daemon and the watchers up (`status` lists the watchers but derives the state without them) | on: no ESTOP, no hold |
| `standby` | the same parts up | off: the hold and the ESTOP are set |
| `off` | Dolt up (unless `--dolt`); daemon, watchers and Forgejo stopped | off |

`gt power status` prints every part and the derived state read-only. It answers
`standby (draining: N polecat(s) working)` while workers finish, and names a
partial standby when one of the two brakes is set without the other.

The `ring:` line beside the hold and ESTOP lines reports the One's signed lease.
Read `gt power status` for what that line says; this page does not repeat it.
It is read-only information until the Ring becomes the brake: no power state
reads it yet, so a valid Ring changes nothing above.

The **hold** (`seat-refill.hold` at the town root) parks every automatic
dispatcher. The **ESTOP** parks dispatch and makes the supervisor refuse
restarts. `standby` sets the hold first and the ESTOP only after the drain:
the ESTOP refuses the restart a polecat may need while it is still landing.
`full` clears the ESTOP (through `gt thaw`, which nudges the held sessions)
before it clears the hold, and leaves the hold in place if a part did not come
up.

## The drain

`standby` and `off` wait until three readings are zero:

- **landing queues**, summed from the daemon's health report;
- **working polecats**, from `gt polecat list --all --json`;
- **a Dolt maintenance gc** (`daemon/dolt.pause`, actor
  `daemon/scheduled_maintenance`).

An unreadable reading is not zero: a queue that cannot be read, a polecat list
that fails, or an unreadable pause marker keeps the drain waiting. A drain
that reaches `--timeout` (seconds, default 900) names the pending landings,
the working polecats and any running gc, and exits 3. Nothing is killed and
no part is stopped: re-run the command to keep waiting.

`off` refuses to start when Dolt is down, because the landing queues cannot be
read and the drain cannot be verified. This refusal applies under `--dry-run`
too, so a preview never shows a stop the real command would refuse. A drain
that finds Dolt unreachable when it runs (for example, Dolt died mid-standby)
fails with exit 3 and names Dolt instead of skipping the wait: an unreadable
queue is not an empty one.

These states never run `gt down`, `gt shutdown` or `--nuke`: those delete
polecat worktrees, which is not a shutdown any power state performs.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | the state was reached, or status was read |
| 2 | usage: a rejected flag, an unreadable `settings/config.json`, or `off` without `--yes` / `--force-cloud` |
| 3 | the drain timed out; the hold is set and nothing was killed |
| 4 | a part did not come up healthy, or Dolt was down when `off` started |

## Hooks

- **jobhost**: `standby` and `off` run `jobhost down` when the binary is on
  PATH (the devops kind cluster), and do nothing when it is not. A failing
  `jobhost down` is reported and does not undo the state that was reached.
- **cloud release bot**: while a `local.gt-cloud.releasebot*` LaunchDaemon is
  installed, `off` needs `--force-cloud`. The bot and patrol poll Forgejo, so
  a stopped stack pauses deploys and makes those jobs log read errors; prefer
  `standby` while an app is live.
- **Docker Desktop**: `full` and `standby` open Docker when the engine is down
  and wait up to four minutes for it; a stack with fewer than four running
  services is started with `docker compose up -d`.

## The Forgejo stack directory

The stack is its own git repo and does not move. Its directory comes from
`settings/config.json`:

```json
{ "forgejo": { "dir": "~/forgejo" } }
```

The default is `~/forgejo`. `gt power` runs `docker compose` in that
directory and probes `FORGEJO_URL` (default `http://forgejo:3000`).

## Reboot to standby

A reboot should land in standby, not full. The login job template is
`internal/templates/launchd/com.gastown.power-boot.plist`: it runs
`gt power boot-hold` at load, which writes the hold and records
`standby (boot)` in `<town>/power-state`. Install it as
`~/Library/LaunchAgents/com.gastown.power-boot.plist` with `GTPath` and
`TownRoot` filled in, then `launchctl bootstrap gui/$(id -u)` it. An operator
clears the boot hold with `gt power full`.

## Outside the states

The cloud-deploy LaunchDaemons (`local.gt-cloud.*`) are outside every state;
`gt power status` counts them and `off` checks for the release bot among them.
The dashboard, cmux and the opt-in local model servers are not part of any
state either.

## Updating this page

`gt power` replaced the operator's shell script of the same states (gt-xagpa).
When a state's ordering or the drain changes, change this page in the same
commit.
