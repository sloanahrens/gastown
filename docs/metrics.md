# Moving a component: metrics checklist

Before you move a component to another host or cluster, follow these three steps so the town's metrics follow the move. Every step is additive: the log keeps its old rows, and a reader can still join rows from before and after the move.

## Step 1: update placement

Edit `<town>/settings/placement.json`. The operator installs this file at runtime; nothing in the repo writes it. `internal/metrics/testdata/placement.example.json` shows the initial content.

- Set the component's value in `places` to the new place. Places are free text in the town's own names, such as `mac-host`, `docker-desktop`, `town-cluster` or `tenant-cluster`. A new place needs no code.
- Set `updated` to the date of the move.
- Keep `"v": 1`. The decode is strict: an unknown field, another version, an empty name, or data after the object is an error.

Done when the next snapshot row shows the new place and has no `errors` entry named `placement`. A bad file leaves the row's placement null and adds that entry. A missing file is not an error; the receipt then says `placement: none known`.

## Step 2: record a regime event

Append one entry to the JSON array in `regime-events.json`. Its default location is `~/.claude/docs/metrics/`; `TOWN_METRICS_DOCS` overrides the directory. The operator writes this file at runtime, outside the repo.

```json
{"date": "2026-10-24", "what": "<component> moved from <old place> to <new place>", "source": "<where the move is recorded>", "confirmed": false}
```

- `date` is the day the move took effect, as `YYYY-MM-DD`.
- `what` says the change in one line.
- Leave `confirmed` false until the operator confirms it. The backtest marks an unconfirmed event `[unconfirmed]`.

Done when the array parses and holds the new entry, and no earlier entry was changed.

## Step 3: add a source only if the move needs a new measurement

A source fills one column group of the snapshot. Add one only when the move needs a value the log does not record yet. Otherwise skip this step.

- The hourly log is written by `gt metrics snapshot`, which the town-metrics plugin runs. A Go source is a `source_<group>.go` in `internal/metrics/`, registered in `Sources()` in `internal/metrics/sources.go`.

Done when the new source's columns appear in a snapshot row, and `git diff` shows the source as the only change.

## The additive rule

Never rename or remove a column in the log. The join and the backtest read columns by name: a snapshot group becomes `snap_<group>_<column>`, and placement becomes `snap_place_<component>`. A rename splits one series into two. A removed column leaves every later row without a value for it.

- A new meaning gets a new column name, even when it replaces an old one. Keep the old column in the log.
- A column that stops applying stays in place. Its later rows carry no value for it.

## Checklist

- [ ] `placement.json` names the new place and carries today's `updated` date
- [ ] `regime-events.json` has the new entry, with `confirmed` false
- [ ] The next snapshot row shows the new place and no `placement` error
- [ ] No column was renamed or removed
