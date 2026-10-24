# Town metrics 2026-10-23

## Headline

Last four weeks against the four before (town, all rigs).

| Metric | Last 4 weeks | Previous 4 weeks |
|---|---|---|
| size_median | 45 | n/a |
| file_entropy_mean | 1.37 | n/a |
| rejected_share | 0.2 | n/a |
| rho | 0.375 | n/a |
| mode_shift | 1.8 | n/a |

## Moved since last run

- ALL 2026-10-12 size_median: 50 (history mean 40, sd 2)

## Metric 1: Landing size

Lines added plus deleted per landing: median and 90th percentile, with landings per week.

| Week | landings | unreadable | size_median | size_p90 |
|---|---|---|---|---|
| 2026-10-05 | 12 | 0 | 40 | 120 |
| 2026-10-12 (thin) | 8 | 1 | 50 | 130 |

Caveat: Landings with an unreadable commit are counted in `unreadable`, never dropped silently.

## Metric 2: File and package entropy

Shannon entropy, in bits, of lines changed across files and across directories, averaged over the week's landings.

| Week | file_entropy_mean | package_entropy_mean |
|---|---|---|
| 2026-10-05 | 1.23 | 0.5 |
| 2026-10-12 (thin) | 1.5 | 0.75 |

Caveat: 0 for a single-file landing. A rename counts as one file.

## Metric 3: Mode shift

Distance between consecutive weeks of the z-scored median size, file entropy and rework share.

| Week | mode_shift |
|---|---|
| 2026-10-05 | n/a |
| 2026-10-12 (thin) | 1.8 |

Caveat: Thin weeks (fewer than 10 landings) are reported but never flagged.

## Metric 4: Rework depth

Attempts per landed bead, share rejected before landing, and share approved first try.

| Week | attempts_mean | rejected_share | first_try_share |
|---|---|---|---|
| 2026-10-05 | n/a | 0.1 | n/a |
| 2026-10-12 (thin) | n/a | 0.3 | n/a |

Caveat: Beads abandoned before landing are absent; the denominator is landed beads.

## Metric 5: Label entropy

Entropy, in bits, of the labels, rigs and issue types of beads created that week.

| Week | h_labels | h_rigs | h_types |
|---|---|---|---|
| 2026-10-05 | n/a | n/a | n/a |
| 2026-10-12 (thin) | n/a | n/a | n/a |

Caveat: Three separate figures, not blended. Labels with fewer than 3 uses fold into other.

## Metric 6: Handoff size

Bytes of notes plus comments on handoff-labelled beads closed that week, and sessions per bead.

| Week | handoff_bytes_total | handoff_bytes_median | sessions_median |
|---|---|---|---|
| 2026-10-05 | n/a | n/a | n/a |
| 2026-10-12 (thin) | n/a | n/a | n/a |

Caveat: Includes beads in `~/.claude`.

## Metric 7: rho

(Non-daemon landings plus crew or handoff beads closed) divided by daemon-dispatched landings.

| Week | rho_numerator | rho_denominator | rho |
|---|---|---|---|
| 2026-10-05 | n/a | n/a | 0.5 |
| 2026-10-12 (thin) | n/a | n/a | 0.25 |

Caveat: Numerator and denominator are both published. The proxy for human-planned work is crew and handoff beads.

## Metric 8: Dependency density

Mean outgoing dependency count of beads closed that week.

| Week | dependency_density |
|---|---|
| 2026-10-05 | n/a |
| 2026-10-12 (thin) | n/a |

Caveat: Outgoing edges per bead; `bd list` gives counts, not edges.

## Metric 9: Non-conservative merges

Rejections of kind conflict or gate as a share of all rejections, and the count of landings that were not daemon-routed.

| Week | rejections | conflict_gate_share | unknown_share | manual_rescue_count |
|---|---|---|---|---|
| 2026-10-05 | n/a | n/a | n/a | n/a |
| 2026-10-12 (thin) | n/a | n/a | n/a | n/a |

Caveat: Kinds come from the kind word in the bead's rejection notes; an unrecognised word counts as `unknown`. Only beads that eventually landed are counted, in the week they landed.

## Regime events

- 2026-10-10: flash to Sonnet

## Usefulness backtest

Not run.

## Mango findings

Not run.

## Data quality

Rows read:
- ledger: 3
- reviews: 5

Rows dropped: 1
- ledger:gastown: unparseable json

Other gaps: 0

Sources unreadable: bd:/x
