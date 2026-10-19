> Status: historical (2026-10). Merged in: gt-mukgk. Not maintained.

# Seat map: one source of truth for polecat models

Epic: gt-mukgk. Decisions: three grilling rounds with Sloan on 2026-10-19 (18 answers), then design sections 1 to 6 approved.

## Problem

The town can run only two polecat models at once, and the setting for which model runs is spread over six places:

- `polecat_pool.agent` and `max_seats` (the pool seat), and `pro_agent`, `pro_max`, `pro_label` (a reserved seat);
- `daemon.patrols.spec_dispatch.hooked_agent` and `max_hooked` (the second seat);
- `role_agents.polecat`;
- an `agent` pin on each deep-review entry in `daemon.patrols.scheduled_slings`;
- the polecat and deep-review roles in `settings/models.json`, written by `~/.claude/tools/gt-models.sh`;
- `gt pool mix`, which exists to keep the others in step and refuses a third agent (`poolMixMaxAgents = 2` in `internal/cmd/pool_mix.go`).

History: the two seats come from the local-model design (gt-md4z, 2026-09-18), a free local model first and a paid overflow second. A request for three model classes on 2026-09-23 (gt-xmsqb) could not be expressed, and was worked around with plugin environment variables and operator memory. The fix that would have put seat classes in config (claude-xev) was never finished. Later beads (gt-yrjfg, gt-sf37d, gt-riazi, gt-bcpj5, gt-lwlva, gt-jdnab) each patched one corner. gt-lwlva's own comment says setting the mix by hand took six commands with three traps in them.

The dispatcher core is already ready for any number of seats: since gt-kr91q, `specdispatch.Budget` holds `Seats []Seat` and draws at random among the free ones. The two-seat limit lives in the configuration and its readers.

## Goal

One map says which models run, how many of each, and when. Any number of agents can run at once. Every reader goes through one loader, so the readers cannot drift apart again.

## Configuration

The map lives in `polecat_pool`, beside the dispatch policy keys it already sits next to:

```json
"polecat_pool": {
  "seats": {
    "claude-sonnet":  {"cap": 3},
    "claude-haiku":   {"cap": 3, "names": ["kestrel", "merlin"]},
    "deepseek-flash": {"cap": 3, "closed": "20:00-08:00"}
  },
  "max_total": 9,
  "timezone": "America/Chicago",
  "min_spawn_gap": "4m",
  "max_priority": 2
}
```

- Each key of `seats` is an agent name. Its command, arguments and environment stay in the `agents` map in the same file; the presets in `internal/config` are fallbacks for names the town does not define.
- `cap` is the number of live polecats on that agent. 0 closes the seat.
- `closed` is an optional daily window, `HH:MM-HH:MM`, in `timezone`. The start is inclusive and the end exclusive. An end earlier than the start wraps past midnight.
- `names` is an optional list of polecat names for that seat.
- `max_total` caps live polecats across every seat. Absent means no total cap.

## Loader

One function, `config.Seats(settings)`, returns the seats as a typed list (agent, cap, window, names), sorted by agent name so every reader and report sees the same order. Every reader calls it. It rejects, at load:

- an agent that is neither in the `agents` map nor a built-in preset;
- a negative cap;
- a window that does not parse, or a `closed` with no valid `timezone`;
- the same name in two seats' `names` lists.

## Dispatcher and roster

- `specBudgetFromConfig` builds `Budget.Seats` from the loader. A seat inside its window stays in the list and counts its live polecats, but takes no new spawn.
- A bead with no `agent:` label goes to an agent picked uniformly at random among the agents with a free, open seat. Uniform per agent, not per free seat, so a small cap still gets work.
- A bead labelled `agent:<x>` goes only to `x`'s seat. When that seat is closed or full, the bead waits, and the tick says so: `closed until 08:00` or `full`.
- The roster counts live polecats per agent from the agent each seat was spawned on, which the agent bead already records, not from a role default (the gt-2q283 class).

## CLI

- `gt pool show` prints one line per agent (cap, live, open or closed, and when a closed seat next opens) and the total.
- `gt pool set claude-haiku=2 deepseek-flash=3` changes the listed agents and leaves the rest. `--total N` sets `max_total`; `--remove <agent>` drops a seat.
- `gt pool mix` is removed; running it prints the `gt pool set` line that does the same thing.
- `gt pool migrate` converts an old file (see Migration).

## Deep reviews

A scheduled entry with `dispatch: "spec"` can no longer carry an `agent`. The spec dispatcher gives the review a seat like any other bead, so deep reviews run on all models. om's reviewer model is not a polecat seat: it is set in om's own config, `~/.config/om/config.json`.

## Landing record and names

- The landing worker writes `agent: <x>` into each landed record, from the seat's agent bead. The KPI tool and the dashboard count landings, rejections and rework per agent from it.
- A name drawn for a seat is bound to that seat's agent and keeps it (the binding from gt-m9dty). A seat with no `names` list draws from a themed default list. Names already in use keep their slots and are not renamed.

## Migration

These keys are rejected at load, each with a message that names the replacement and says to run `gt pool migrate`:

- `polecat_pool.agent`, `max_seats`, `pro_agent`, `pro_max`, `pro_label`;
- `daemon.patrols.spec_dispatch.hooked_agent`, `max_hooked`;
- `role_agents.polecat`;
- `agent` on a `dispatch: "spec"` entry in `scheduled_slings`.

`gt pool migrate` backs up `settings/config.json`, writes `seats` from the old keys (the pool seat and the second seat each become an entry with their cap), removes the old keys, and prints what it changed. `--dry-run` writes nothing. A second run finds nothing to do.

The cutover is one operator step on a quiet town: land the code, run `gt pool migrate`, restart the daemon. From that restart the old keys no longer load.

## What is retired

- The polecat and deep-review roles in `settings/models.json`, and `gt-models.sh` as the way to set them.
- The `gt-models.sh approve` record. The rule that a model beyond the ordinary ones needs Sloan's approval stays in the `/gastown` handbook as policy.
- `role_agents.polecat`. The other roles keep their `role_agents` entries.

## Error handling

- A config that fails the loader stops the daemon at start with the loader's message. It never runs with a partial seat list.
- A tick never falls back to another agent for a pinned bead.
- `gt pool set` validates the whole resulting map before it writes, and writes nothing on an error.

## Testing

- Loader: a table test per rejection rule, and one for the sorted order.
- Dispatcher: seeded random source; pinned bead with closed, full and open seats; uniform choice across agents with unequal caps.
- Windows: an injected clock at 07:59, 08:00, 19:59, 20:00 and across midnight.
- Strict decoding: one test per retired key, checking the message names its replacement.
- `gt pool migrate`: golden files for each old shape, and a second run that changes nothing.

## Docs

The `/gastown` handbook, `~/.claude/docs/gastown-operations.md` and `docs/guides/spec-dispatcher.md` change in the same epic. The handbook drops `gt-models.sh` and `gt pool mix`.

## Beads

- Closes claude-xev, the unfinished seat-classes-in-config fix.
- Takes in gt-000pd (DeepSeek overnight window; its branch is `polecat/furiosa/gt-000pd+mvficrlz` at cfa1c4af) and the pool part of gt-xarrl (the haiku pin and town definition are already done).
- gt-m9dty (model-bound names; branch `polecat/rictus/gt-m9dty+mvfi1zkp` at d7b85ed2) supplies the name binding.
