# Jev experiments: protocol and idea index

Owner: egret (handoff bead `gt-egret`). Operator decisions of 2026-10-30: collect through 2026-11-20 with the current batteries (option A), be expansive about ideas because Jev calls are cheap to test, be methodical about what gets decided. This page is the one place an idea is defined, measured and judged. A new idea adds a row to the index table below. It does not get a new document.

Background: `docs/reference.md` describes each battery, `gt jev scorecard` grades them, and the index bead is `gt-b2gu8`. Jev is the Typesafe Choice API (`internal/jev`, model `jev-1.13.0`, pinned through 2026-11-20).

## The protocol

Every idea goes through the same seven steps, in this order. A step that cannot be filled in means the idea is not ready.

1. **Hypothesis.** One sentence a result can contradict: "Jev's answer to Q predicts outcome O better than the base rate."
2. **Log-only collection.** The battery asks and writes its answers to its log. Nothing reads them to act. A new battery or question is a bead of its own.
3. **Metric and floor.** The scorecard's rule, applied per question: at least 10 positives, at least 10 negatives, precision at the best cut of at least 0.3, a best F1 at least 0.1 above the base rate, and (once `gt-dx8t2` is installed) at least 10 items flagged at the best cut. The numbers live in `internal/jev/scorecard/scorecard.go`; this page does not copy them again.
4. **Cost per day.** Calls per day times the ledger's average, from `gt jev spend --usd-per-billion-tokens 42`. The patrol cap of 1 USD a day is shared; patrols peaked at 0.53 USD on 2026-10-29.
5. **Privacy flag.** What text leaves the machine (see "Privacy" below). An idea that sends passages, transcripts or secrets that the existing batteries do not already send waits for the scrub bead.
6. **Review date and decider.** The date the row is read. The overseer reads results and reports. The operator decides anything that acts, and sets its threshold.
7. **Decision rule.** Earning: the floor is met, so the idea may be proposed as a consumer. No-signal: enough data, no lift, so the battery is retired or reworded. Not-yet: not enough data, so keep collecting to the next review date. Three reviews of not-yet with no growth in labeled items retire the idea.

### Acting-threshold policy (proposed; the operator sets the numbers)

A question that earns still needs a threshold before anything acts on it. The vendor's guidance is that the threshold "scales with risk" and should be set from labeled examples and the cost of a wrong decision. Proposed classes:

| Class | What a wrong answer costs | Bar |
|---|---|---|
| Display order | A misplaced row | Precision at the cut at least 0.5 on the question's labels |
| Advisory flag | A person looks and moves on | At least 0.6, and a noise band from the repeat measurement (I2) around the cut |
| Extra review | Reviewer time | At least 0.6, only ever adds review, never skips it |
| Block, stop or skip | Lost or wrong work | At least 0.85, and a human confirms |

The bars are a starting point from the vendor's examples (0.6 and 0.85) and need the operator's confirmation. Calibration (I1) replaces them with measured values.

## Privacy

Every state goes to the hosted service (`api.typesafe.ai`). The vendor says it will not train on queries; LangChain's integration page tells users to remove secrets first. What the existing batteries send, from the builders in `internal/jev/*` (read 2026-10-30; the exact text is not logged until `gt-s0tsg` lands):

| Battery | Sends |
|---|---|
| hm reranker | query and 30 passages, each cut to 800 characters |
| logtriage | grouped daemon log lines |
| rules, progress | a tail of the seat's session transcript |
| failcause | a tail of a failed test log |
| submission, landing, dispatch, thrash, conflict, stale, dedupe | bead title, goal, acceptance, MR notes, commit subjects, file stat (diff stat, not diff text) |
| drift | doc sections and diff hunks |
| attention | item summaries |

Transcripts and log tails can hold a path, an id or a secret that a scrub would remove. The scrub bead for hm is proposed in the Jev Report; until a scrub exists, no idea adds a new kind of text to this list. The dataset export below sends nothing to Jev: it reads logs the town already holds and writes ids and features only.

## Dataset for mango

Mango (the HyperCube pattern finder, `mango` rig) can mine the Jev logs for patterns the scorecard does not show. `gt jev dataset` (bead `gt-21wvf`) writes one CSV per battery question. The format was agreed with merlin on 2026-10-30 against mango's loader.

- Location: the town runtime's `jev-dataset` directory, files `<battery>.<question>.csv` and `<battery>.<question>.dict.md`.
- Row: one logged, answered item that has a label.
- Columns: `row_id` (opaque hash; mango ignores it), `day`, `hour`, `dow`, `model`, `answer`, `answer_band`, `confidence`, `label` (the words true or false, the mango target), `label_rule`, `label_source`, then `ref_<name>` for allow-listed refs (rig, agent, attempt, target) and `ans_<question>` for every other answer on the same item.
- Rules: ids and features only. No prompt, passage, title, transcript or log sample. A deny-list is a test. A categorical column with more than 15 distinct values is dropped and named in the dict. Rows are written in a deterministic shuffled order because mango infers column types from the first 100 rows. Empty cell is missing.
- Mango side: merlin runs mango on the files in a scratch database and reports patterns with the sample-size caveat. Mango never writes to the town.

## Index

Status values: row (proposed, nothing built), bead filed, collecting, reviewed. Cost is per day, from the 7-day ledger (about 42,000 calls for 4.14 USD).

| ID | Idea and hypothesis | Collection (log-only) | Metric and floor | Cost a day | Privacy | Review and decider | Mango question | Bad idea if | Bead |
|---|---|---|---|---|---|---|---|---|---|
| I1 | **Calibration per question.** Jev's probabilities are monotone in the label rate. | None new: offline from existing labels | ECE, Brier and a monotone flag per question; reports only | 0 | none new | 2026-11-20, overseer reports | Which subgroups (category, hour, rig) are over-confident | The numbers add noise and no threshold changes | `gt-km2b8` |
| I2 | **Answer noise.** Re-asking the same state moves the answer by less than the width of the uncertain band. | Logtriage: 2% of groups, 5 repeats, repeat file | Share of items whose repeats cross the cut; mean and max standard deviation | about 0.014 (pilot) | none new (same text re-sent) | One week after install, overseer | Which items are unstable (their features) | Noise is so large no threshold is usable, which is itself the result | `gt-sqyk5`, `gt-4ins7` |
| I3 | **Threshold by cost of error.** A written bar per consumer class beats one global floor. | None | The table above filled with measured values from I1 and I2 | 0 | none | 2026-11-20, operator | none | The classes do not match real consumers | in this page |
| I4 | **Stuck-detection labels.** Overrun stops and scope findings can score `looping` and `drifted_from_bead`. | Existing progress battery | Floor, per question | 0 | none new (progress already sends transcript tails) | 2026-11-20, overseer; acting use is the operator's | What separates looping runs from landed runs | The scope-word proxy labels mostly noise; judge by the scorecard | `gt-tpv79` |
| I5 | **Escalate-only review depth.** High submission risk predicts om findings. | Existing submission and landing batteries | Floor on the risk and claims questions against om verdicts | small (extra reviews only) | none new | After 2026-11-20, operator | Which bead features predict an om rejection | Flagged volume exceeds what a reviewer can read | `gt-xcyai` (deferred) |
| I6 | **Route work to seats.** Difficulty and risk answers predict first-try landing by seat. | Existing dispatch battery; needs per-seat outcomes | Floor on `needs_strong_model` and `fits_twenty_minutes`, with a hold-out | engineering | none new | After a floor check, operator | Which bead features separate runs a weak seat fails | A misrouted run costs more than the seat saving | `gt-zytdq` (deferred) |
| I7 | **Intent routing for hm search.** Change queries (recall 0.68) improve when classified. | New question on the rerank request, if the request shape allows | hm eval recall for change queries | 0 to about 0.5 | query only (already sent) | After I1 and I2, operator | none | Change-query recall does not move | `gt-zfqss` (deferred) |
| I8 | **Context selection for prompts.** Choosing which directives and docs enter a prompt cuts wrong guidance. | None yet | No clean label; proxy is an om finding citing a rule the prompt lacked | small | **sends directive and task text; needs the scrub bead first** | Not scheduled | none | Prompts shrink but first-try landings fall | `gt-pvyce` (deferred) |
| I9 | **Gate every tool call.** A fast judge stops destructive tool calls. | None. Parked since 2026-10-20 | Would need a label from guard-hook blocks | thousands of calls a day | **sends tool arguments; needs the scrub bead first** | Parked by operator decision 2026-10-30 | none | Always, until a local-only design exists | `gt-b2gu8` |
| I10 | **Wording experiments.** Alternate wordings of a weak question score better on held-out labels. | Replay of logged inputs (needs `gt-s0tsg`) | Scorecard on held-out items | about 0.1 to 0.4 per experiment | replays text already sent | After `gt-s0tsg`, overseer | none | Gains vanish on fresh data | `gt-n83oz` (deferred) |
| I11 | **Composite priority.** Severity, risk and urgency combine with weights kept in config. | None; needs an urgency label | Fit against acks | 0 | none | After I4, operator | Which score combination best predicts an ack | A single score fits as well | `gt-w1cxc` (deferred) |
| N1 | **Second opinion on om findings.** A finding that a later rework reverses is a misread. | Would ask Jev whether each om finding holds | Floor against rework outcomes | small | **sends findings and diff excerpts; needs the scrub bead first** | Not scheduled | Which finding types are reversed | om already scores its own findings well enough | row only |
| N2 | **Bead shaping quality.** Jev's `acceptance_tickable` answer predicts a polecat refusing to tick a box. | Existing dispatch battery; needs the label | Floor once a label rule exists (gt done refusals) | 0 | none new | After a label rule exists, overseer | Which bead features predict an unticked box | `gt spec lint` already catches the same cases | row only |
| N3 | **Early stuck warning.** Asking progress at half the overrun limit predicts the later stop. | Move one existing ask earlier, log-only | Floor on `looping` against overrun stops | small | none new | After I4, overseer | Which runs warn early and stop late | The early ask matches a plain elapsed-time rule | row only |
| N4 | **Rejection cause mix.** The rework battery's `cause` question, which has no label rule by design, shows which causes cluster by rig, size and seat. | Existing rework battery | Descriptive only; no floor | 0 | none new | After the first dataset, merlin reports | Which causes cluster by rig, bead size and seat | The mix is flat | row only |

## Adding a row

1. Write the seven protocol steps as the row; leave nothing blank (write "none" where it applies).
2. State what would make it a bad idea; a row without that column does not get a bead.
3. Check privacy: if it adds a new kind of text to the table above, mark the row and block it on the scrub bead.
4. File one bead per build step: shaped and released only when it is log-only and its dependencies have landed; otherwise a deferred stub assigned to egret, labeled `operator-only` if it would act.
5. Put the bead id in the Bead column. Dated results go in the row's review comment on the bead, not in this table.
