> Status: historical (2026-10). Merged in: gt-sxg5u. Not maintained. The replay was replaced by `gt jev attention` plus `gt jev scorecard --battery attention` (gt-3ey5j).

# Jev advisory triage for the attention queue (pilot design)

Bead: gt-sxg5u.

## Goal

Suggest a disposition for each attention item so the overseer sees the items that need a human first. Jev answers a single Choice question per item: `needs-you`, `auto-fixable` or `noise`. Its answer is a suggestion shown beside the item. It never acks, hides, wakes or changes an item.

Attention items come from `<town>/.runtime/attention/`. `state.json` holds the current set, `events.jsonl` (rotated to `events.jsonl.1` at 1 MB) holds every new and cleared transition, and `acks.json` holds acknowledgements. Each item carries `key, kind, severity, rig, bead, sha, summary, first_seen, last_seen, acked_at`.

## Approach (chosen: advisory sidecar)

A separate job reads the queue and writes its own file, `<town>/.runtime/attention/jev-triage.json`. `gt attention` reads that file and adds a suggestion column. The daemon, `gt attention ack`, and the wake stream are unchanged.

Rejected for the pilot:
- **Auto-ack noise.** An ack hides an item until its condition clears. A wrong call would hide a real problem.
- **Gate the wake stream.** Fewer interruptions, but a miss becomes silence. Revisit after calibration.

## Components

1. **Triage classifier** (`internal/attention/jevtriage`): for each item, build a state with the item's kind, severity, rig, bead and summary, and ask one Choice question. It returns the label, the probabilities and the confidence, and writes them to `jev-triage.json` keyed by item key and `last_seen`.
2. **Display**: `gt attention` gets one extra column, `jev`, showing the label and confidence. Items with no answer show `-`.
3. **Config**: `attention.jev_triage.enabled` (default false), `model` (pinned `jev-1.13.0`), `confidence_floor`, and `max_spend_usd` (pilot cap: $5).
4. **Replay tool** (pilot): reads `events.jsonl` and `events.jsonl.1` and classifies every item it finds, writing a report.

## Fail-safe rules

- An item whose `kind` is `escalation` (the kind the queue gives items raised by `gt escalate`), or with a confidence below `confidence_floor`, is always shown as `needs-you`, whatever Jev said.
- A parse failure, a timeout or a 429 writes `needs-you` with reason `no-answer`.
- Jev may move an item away from `needs-you` only when no rule above applies. Jev can never hide an item.
- The summary goes into the state as data. The prompt says so, and the answer is parsed against the schema only.

## Pilot and measures

Ground truth is defined in "Measuring the pilot" below.

The pilot replays the full event history offline, read-only, and reports:
- precision and recall of `needs-you` against the proxy;
- calibration: stated confidence against proxy accuracy, by bin;
- how many items the fail-safe rules forced to `needs-you`;
- the actual spend against the $5 cap.

The pilot report goes to Sloan. Turning the triage on live needs his approval.

## Measuring the pilot

The pilot has two tiers of ground truth. The tiers are reported separately.

**Tier 1, what exists today.** `events.jsonl` (and `events.jsonl.1`) records every new and cleared transition. An ack is not recorded there, and `acks.json` drops an ack when its item clears. So tier 1 can say that an item cleared, but not whether a human acted on it first. Tier 1 therefore gives no ground-truth label. It is used only to check counts against tier 2. This is only possible for items whose ack survives until they clear, which is why tier 1 is weak.

**Tier 2, the ack history (gt-s1xj6).** `gt attention ack` appends one line per ack to `acks-history.jsonl` in the attention directory, written under the existing acks lock and rotated at the events size limit. Each line is `{ts, key, kind, rule}`. The history outlives the ack and the item, so the replay can label an item by what the overseer did, even after it cleared. The replay uses tier 2 as the primary label and reports tier 1 alongside it, so the difference between the two shows how much tier 1 was missing.

The pilot does not start until gt-s1xj6 has landed. Until then, the replay reports tier 1 only and says so.

## Cost

Input is $42 per billion tokens. At an estimated 500 tokens per item, the full history of about 1,700 events comes to roughly $0.04 per replay, so the flat $5 cap sits well above it. The pilot records the real token counts and replaces this estimate.

## Error handling and testing

- The triage job is advisory, so a crash or a stale file leaves `gt attention` working. It shows `-` for the suggestions.
- Unit tests: the fail-safe rules, the no-answer path, the summary-as-data prompt, and the schema parse.
- A replay test on a fixed events fixture checks the report's shape.

## Out of scope

- The other three uses (deep-review dedupe, P3 severity, risk-diff second opinion). Each gets its own design after this pilot.
- Any change to ack semantics, the daemon, or the wake stream.
- Landing or security decisions.

## Decisions settled in grilling (2026-10-19)

- First use: attention-queue triage.
- Measure offline before anything reads the output.
- Code in the gastown rig, behind a config flag that defaults to off.
- Pin `jev-1.13.0`, not the preview alias.
- Pilot spend cap: a flat $5, well above the replay estimate of about $0.04.
