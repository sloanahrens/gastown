> Status: historical (2026-11). Merged in: docs/plans/2026-11-11-story-points-design.md. Not maintained.

# Story points retrospective (eagle, 2026-11-11)

For gt-5d9k2f.1, parent epic gt-5d9k2f. Read-only on the town. Every number below was computed today from the files named in "Data and method"; what I could not check is in "Not verified".

## Summary

1. **The measure.** The cleanest source of "polecat wall-minutes to land, across every session and rework round" is the landing record's `polecat_secs`, which the landing worker has written since 10-26 (2,454 landed beads, 2,297 of them from the two seat classes that matter). The older usage records (`*.work.json`) measure a different, smaller number and stop on 10-29. I used the landing record and kept the usage records only as a secondary view.
2. **The spread.** Per bead, the median is about 7 minutes on the Haiku seat and 9 on the DeepSeek seat. The top 5% take more than 40 minutes. A bead needing more than one om round takes about three times as long as one that lands first time (6 min against 18 for two rounds, 32 for three).
3. **The cutpoints.** (The operator later chose Fibonacci midpoint edges, section 7, which this evidence supports.) The provisional cutpoints are usable. Moved to the measured quantiles they become 1 up to 4 min, 2 up to 7, 3 up to 11, 5 up to 17, 8 up to 28, 13 up to 45, above 13 over 45. One scale for both seat classes, with the class recorded on every row.
4. **The estimator.** The epic's first-named estimator, hm nearest-bead analogy, fails the bar: rank correlation 0.27 to 0.29. Jev's `fits_twenty_minutes` answer, a shape-features model, and (added after the first report) a ridge regression from the embedding of the bead's Constraints and Acceptance text each clear 0.4. An average of the shape model and `fits_twenty_minutes` reaches 0.64 (95% interval 0.57 to 0.69, 371 beads), and adding the embedding ridge reaches 0.67 (0.61 to 0.72). The embedding ridge alone is 0.57, as good as Jev, with no model call and no cost.
5. **The bar for routing.** The 0.4 bar is cleared by every blend with room to spare. The holdout is four days, so shadow mode should confirm it on the live data. The split rule is a different matter: no blend ever predicts more than about 20 minutes, so a rule of "estimate over 21 points" never fires, and the best lower thresholds flag four beads that run long for every six that do not (see section 7).

## Data and method

**Sources.** All reads are of copies under `~/gt/.runtime/eagle/sp/`.

| Source | What I read | Which copy |
|---|---|---|
| Landing ledgers, `.runtime/landings/<rig>.jsonl` | 6,408 distinct landed beads, 2026-09-30 to 2026-11-11 | The pod's claim copy (4,964 gastown lines against the Mac's 4,755). Copied 11-11. |
| `reviews.jsonl` | 6,821 om verdict rows (attempt, score, bar) | The pod's copy |
| Jev dispatch log, `.runtime/jev/dispatch.jsonl` | 4,576 rows, 3,385 beads | The pod's copy |
| Usage records, `.runtime/usage/beads/*.work.json` | 5,462 files, 2,408 with wall time, polecat starts 10-05 to 10-29 | The Mac's copy. The pod holds the same 5,462 files; nothing newer exists. |
| hm Postgres, `documents` and `chunk_embeddings` | 9,457 bead documents with the first-chunk embedding of the `qwen3-embedding:4b-q8_0` model | The live database. Its last sync was 11-06 (122 hours before the read), so beads landed after 11-06 are not in it. |

**Rows.** One row per landed bead routed through the daemon (6,296; the 112 overseer-direct and manual landings are excluded because no polecat time applies). A bead with two landings uses the later one (6 beads).

**Actual minutes.** `polecat_secs` / 60 from the landing record, which sums every attempt's span across seats. A record flagged `polecat_lower_bound` has an attempt whose span never closed, so its minutes are a floor (13% of Haiku rows, 9% of DeepSeek rows). I kept them in and report the share. Where no landing-record time exists, I fell back to the usage record's `wall_s`, and I never mixed the two within one table.

**Seat class.** The landing record's `agent` field (`sbx-haiku` and `claude-haiku` are "haiku"; `sbx-flash` and the `deepseek-*` names are "flash"; `claude-sonnet` is "sonnet"). Where the field is absent I used the model names in the usage record.

**Om threshold era.** The bar recorded on the bead's last review row was 0.75 for every span-era bead that has a review row (2,441 of 2,454). There is no era variation inside the window, so the regime column is constant here.

**Backtest.** A date holdout: train and neighbour pool are beads landed before 2026-11-03; the test set is beads landed on or after it. The hm and shape estimators are tested on the beads that are in the hm index (landed 11-03 to 11-06). Jev is tested on every bead with a Jev answer (landed 11-03 to 11-11), and also on the 371 beads that have both so the three can be compared on one set. A neighbour must have landed before the test bead. Rank correlation is Spearman against actual minutes; intervals are 400-sample bootstrap percentiles.

## 1. Actual minutes per landed bead

Landing-record minutes (the measure I recommend), 2026-10-26 to 11-11:

| Class | n | p10 | p25 | median | p75 | p90 | p95 | p99 | mean |
|---|---|---|---|---|---|---|---|---|---|
| Haiku seat | 1,096 | 3.1 | 4.2 | 7.0 | 14.1 | 28.3 | 38.5 | 75.1 | 13.4 |
| DeepSeek seat | 1,201 | 3.7 | 5.4 | 8.6 | 15.9 | 31.8 | 47.1 | 104.9 | 18.2 |

The two classes differ (Mann-Whitney p < 1e-9), but by about 1.5 minutes at the median, less than one bucket.

Usage-record minutes, 2026-10-05 to 10-29, for the older era and for the Sonnet seat that has no landing-record time:

| Class | n | p10 | p25 | median | p75 | p90 | p95 | p99 | mean |
|---|---|---|---|---|---|---|---|---|---|
| Haiku seat | 513 | 2.6 | 3.3 | 4.8 | 7.8 | 14.2 | 17.4 | 29.2 | 6.6 |
| DeepSeek seat | 897 | 1.9 | 3.5 | 6.4 | 10.8 | 16.5 | 21.1 | 33.0 | 8.3 |
| Sonnet seat | 904 | 1.0 | 1.6 | 2.8 | 5.6 | 11.3 | 16.3 | 26.1 | 4.8 |

**The two measures are not interchangeable.** Only 20 beads have both; the landing-record time is a median 1.4 times the usage time (quartiles 1.2 and 4.5). I read this as the span counting time the transcript wall clock does not (the gap between sessions, waits), but I did not prove it. Mixing the two would add a seat-era artefact to every estimator, so the backtest below uses landing-record minutes only, and the older usage minutes serve only as a variant of the hm pool (scaled by 1.4; no better).

**Rework dominates the tail.** Minutes against om rounds (rows with a review history):

| om rounds | beads | median minutes |
|---|---|---|
| 1 | 1,649 | 6.1 |
| 2 | 472 | 17.7 |
| 3 | 154 | 31.6 |
| 4 | 12 | 34.0 |

Spearman of minutes with om rounds is 0.59. A before-estimate cannot see whether a bead will be rejected, so this is the main noise floor. In the span era, 17 to 19% of beads exceed 20 minutes and use 57 to 63% of the time; above 40 minutes it is 4 to 7% of beads and 29 to 44% of the time. The 10-13 overrun-rule design recorded 4% of beads over 20 minutes using 16% of the time; the landing-record measure includes rework rounds and waits, which that older figure did not.

**Coverage.** 1,528 daemon-landed beads have no minutes at all (517 of them since 10-26). They are left out; I did not check why each lacks a span.

## 2. Cutpoints

How the provisional cutpoints (3, 6, 10, 15, 25, 40 minutes) spread the 2,297 span-era beads, against cutpoints set to the measured quantiles (10, 30, 50, 70, 85, 95%) of the training period and rounded:

| Points | 1 | 2 | 3 | 5 | 8 | 13 | above 13 |
|---|---|---|---|---|---|---|---|
| Provisional upper bound (min) | 3 | 6 | 10 | 15 | 25 | 40 | over 40 |
| Share of beads, provisional | 6.2% | 30.8% | 23.6% | 14.4% | 11.2% | 8.4% | 5.5% |
| **Proposed upper bound (min)** | **4** | **7** | **11** | **17** | **28** | **45** | **over 45** |
| Share of beads, proposed | 17.5% | 27.3% | 19.7% | 13.7% | 10.3% | 7.1% | 4.5% |

The provisional scale leaves point 1 nearly empty and puts a third of beads in point 2. The proposed scale moves every bound up by 1 to 5 minutes and fills point 1. By class under the proposed scale: Haiku 23 / 28 / 18 / 11 / 10 / 7 / 3%; DeepSeek 12 / 27 / 21 / 16 / 11 / 7 / 6%. The scale is usable for both classes with the classes about one bucket apart at the low end.

**Recommendation.** One scale for both seat classes (a point describes the bead, and splitting the scale by seat would make the same bead a 3 on one seat and a 5 on another). Record the class on every row and recompute monthly. If the operator wants class-specific scales, the quantiles are in the table below.

| Quantile | Haiku (train) | DeepSeek (train) | Pooled (train) |
|---|---|---|---|
| 10% | 3.0 | 3.7 | 3.3 |
| 30% | 4.1 | 5.9 | 5.0 |
| 50% | 5.7 | 8.3 | 7.4 |
| 70% | 9.9 | 14.1 | 13.0 |
| 85% | 20.9 | 26.1 | 24.5 |
| 95% | 37.5 | 48.6 | 44.7 |

"Above 13 means split or overseer-direct" corresponds to over 45 minutes, about 4.5% of beads and about 30 to 44% of polecat time. That is the share of work routing would divert.

**Lower-bound rows** (an attempt never closed) are 9 to 13% of the span-era rows; their true minutes are higher, so the top bucket is slightly undercounted.

## 3. Backtest

Train: the span-era beads landed before 11-03. Test: beads landed 11-03 to 11-06 that are in hm (haiku 191, flash 242), and, for Jev, beads landed 11-03 to 11-11. The estimators:

- **(a) hm analogy.** Cosine similarity of the first-chunk embedding of the bead text before its first note or comment; median of the minutes of the k most similar beads that landed earlier.
- **(b) shape features.** A model on log minutes from: acceptance box count, distinct file names and package paths named, length of the text, constraint bullets, number of labels, `test` mentions, backtick count, rig, seat class. Two forms: gradient-boosted trees, and ridge regression (a linear model). Dependency count is not in the text and is not used. The landing record's `risk_paths` are known only after landing and are not used.
- **(c) Jev.** The dispatch battery's `size.score` and `fits_twenty_minutes`, each mapped to minutes by an isotonic fit on the training period.
- **(d) ridge on embeddings** (section 6).

### Rank correlation with actual minutes, per seat class

Spearman, with the 95% bootstrap interval. The bar is 0.4. "Own set" is every test bead that has the input; the common set is the 371 test beads that have all inputs.

| Estimator | Haiku seat | DeepSeek seat | Both |
|---|---|---|---|
| (a) hm analogy, k=10 (own set, n 191 / 242 / 433) | 0.27 [0.15, 0.42] | 0.28 [0.16, 0.39] | 0.29 [0.20, 0.37] |
| (b) shape, boosted trees (own set) | 0.50 [0.38, 0.60] | 0.43 [0.33, 0.54] | 0.50 [0.42, 0.57] |
| (b) shape, ridge (own set) | 0.50 [0.39, 0.60] | 0.39 [0.29, 0.49] | 0.49 [0.40, 0.55] |
| (c) Jev `size.score` (common set, n 165 / 206 / 371) | 0.41 [0.27, 0.53] | 0.39 [0.28, 0.49] | 0.44 [0.35, 0.51] |
| (c) Jev `fits_twenty_minutes` (common set) | 0.54 [0.43, 0.64] | 0.62 [0.51, 0.70] | 0.57 [0.49, 0.64] |
| (d) ridge on section embedding (own set) | 0.54 [0.42, 0.64] | 0.58 [0.49, 0.65] | 0.57 [0.50, 0.63] |
| (d) ridge on hm first-chunk embedding (own set) | 0.40 [0.26, 0.52] | 0.34 [0.24, 0.43] | 0.38 [0.29, 0.46] |
| shape ridge + `fits_twenty_minutes` (common set) | 0.62 [0.52, 0.71] | 0.61 [0.52, 0.70] | 0.64 [0.57, 0.69] |
| shape ridge + embedding (common set) | 0.62 [0.51, 0.72] | 0.60 [0.52, 0.69] | 0.62 [0.55, 0.67] |
| shape ridge + `fits_twenty_minutes` + embedding (common set) | 0.65 [0.55, 0.74] | 0.67 [0.57, 0.73] | 0.67 [0.61, 0.72] |

For the blends, the inputs are each turned into minutes and averaged on the log scale with equal weight.

On the common set the other estimators read: hm analogy 0.34 / 0.33 / 0.36; shape with boosted trees 0.58 / 0.46 / 0.55.

On the wider Jev-only test set (793 beads, 11-03 to 11-11), `fits_twenty_minutes` gives Haiku 0.56 [0.48, 0.62], DeepSeek 0.60 [0.52, 0.66]; the first half of the window gives 0.58 and the second 0.56. Restricting to beads that landed on the first om round, the common-set correlations are, Haiku / DeepSeek: shape ridge 0.53 / 0.38, `fits_twenty_minutes` 0.50 / 0.59, embedding ridge 0.61 / 0.56, shape + `fits_twenty_minutes` 0.58 / 0.56, all three 0.63 / 0.61. Across two halves of the common test window the three-way blend gives 0.71 and 0.60, the shape + `fits_twenty_minutes` blend 0.67 and 0.55.

### Bucket accuracy

Share of test beads whose estimated bucket is the actual bucket (exact) or one off, with the operator's Fibonacci scale (edges at 1.5, 2.5, 4, 6.5, 10.5, 17, 27.5, 44.5 minutes; section 7). Common set. The last row is the exact share of always guessing the most common bucket.

| Estimator | Haiku exact / within one | DeepSeek exact / within one |
|---|---|---|
| hm analogy, k=10 | 0.29 / 0.68 | 0.21 / 0.65 |
| shape, boosted trees | 0.33 / 0.78 | 0.27 / 0.74 |
| shape, ridge | 0.28 / 0.81 | 0.29 / 0.68 |
| Jev `size.score` | 0.24 / 0.76 | 0.28 / 0.69 |
| Jev `fits_twenty_minutes` | 0.27 / 0.72 | 0.33 / 0.77 |
| ridge on section embedding | 0.32 / 0.79 | 0.27 / 0.75 |
| shape ridge + `fits_twenty_minutes` | 0.27 / 0.79 | 0.33 / 0.76 |
| shape ridge + embedding | 0.33 / 0.81 | 0.31 / 0.77 |
| all three | 0.33 / 0.82 | 0.36 / 0.80 |
| Naive: always the most common bucket | 0.26 | 0.23 |

Exact-bucket accuracy is low for everything (0.2 to 0.4): the estimators order beads well and name the bucket only a third of the time. Within one bucket is 0.65 to 0.82.

### Why each result looks the way it does

- **hm analogy is weakest.** A bead's text says what is asked, not how hard it is. The nearest neighbours often share a topic and not a size. The prior town experiment (`town-rejection-experiment.md`) found the same: dispatch-time columns did not predict rejection, and it checked more columns. The hm index was also five days old, so the neighbour pool lacked the newest beads.
- **Shape features** work through text length, test mentions and box count (importances 0.28, 0.16 and 0.16 of the boosted model).
- **`fits_twenty_minutes`** was designed to predict this very quantity (overruns). Its answers come from a model reading the bead's Goal, Constraints, Acceptance and Size sections (`internal/jev/dispatch`), so they use no information from the work itself.

## 4. Recommendation (first report; superseded in part by sections 6 and 7)

1. Use a blend, not a single input. The shape model and `fits_twenty_minutes` average to 0.64; the embedding ridge added brings it to 0.67.
2. Demote hm analogy. It does not clear the bar (0.29) and stays only as the note that names the nearest beads.
3. Do not use Jev `size.score` alone: DeepSeek 0.39 does not reliably clear 0.4.
4. Shadow mode before routing.
5. Define the actual as the landing record's `polecat_secs` (with its attempts and lower-bound flag), because it is written for every landing and covers every round. The usage records stop on 10-29: the scan log shows "new 0" since, which is consistent with sandbox seats writing no transcript on the Mac, though I did not confirm that cause.

## 6. Added after the first report: ridge regression on embeddings

The operator asked whether a vector-space embedding like hm's could estimate complexity. The nearest-neighbour analogy treats every embedding direction as equally important, so topic dominates. A ridge regression learns which directions carry size.

**Method.** Ridge regression from the 2,560-number embedding to log polecat-minutes. Alpha is chosen by 5-fold time-ordered cross-validation on the training dates only (before 11-03), then the model is scored on the 433 beads landed 11-03 to 11-06 that are in hm. Two inputs:

- **(i) hm's existing first-chunk embedding** of each bead (title and the start of the description). Alpha 3.2.
- **(ii) a fresh embedding** of the bead's Constraints and Acceptance sections only, taken from the text before its first note or comment, with the same `qwen3-embedding:4b-q8_0` model through the embedder hm uses (ollama on the Mac), 1,797 beads, 39 seconds of embedding for texts of nearly empty length and a few minutes for the real ones. Alpha 1.8.

**Result.**

| Input | Haiku seat | DeepSeek seat | Both | Clears 0.4? |
|---|---|---|---|---|
| (i) hm first-chunk embedding | 0.40 [0.26, 0.52] | 0.34 [0.24, 0.43] | 0.38 [0.29, 0.46] | borderline Haiku, no for DeepSeek |
| (ii) Constraints and Acceptance embedding | 0.54 [0.42, 0.64] | 0.58 [0.49, 0.65] | 0.57 [0.50, 0.63] | yes, both classes |
| (i) and (ii) joined | 0.50 [0.37, 0.60] | 0.54 [0.46, 0.62] | 0.53 [0.45, 0.60] | yes |

The fresh embedding of the sections that say what must be built and checked is the best single estimator tested: as good as Jev's `fits_twenty_minutes` (0.57), better than the shape model (0.49 to 0.50), with no model call and no per-bead cost. Embedding the whole first chunk, which is mostly title and goal, carries the topic and not the size.

**Does it improve the blend?** Averaged in on the log scale:

| Blend (common set, 371 beads) | Haiku | DeepSeek | Both |
|---|---|---|---|
| shape ridge + `fits_twenty_minutes` | 0.62 | 0.61 | 0.64 [0.57, 0.69] |
| shape ridge + embedding (no Jev) | 0.62 | 0.60 | 0.62 [0.55, 0.67] |
| shape ridge + `fits_twenty_minutes` + embedding | 0.65 | 0.67 | 0.67 [0.61, 0.72] |
| `fits_twenty_minutes` + embedding (no shape) | 0.64 | 0.71 | 0.68 [0.61, 0.73] |

It beats the shape plus `fits_twenty_minutes` blend by about 0.03, inside the bootstrap interval, so the gain is suggestive and not proven. What the embedding does change clearly: shape plus embedding reaches 0.62 **without calling Jev at all**, within 0.02 of the Jev blend. That removes a paid model call and the problem that Jev's dispatch answers are logged after placement (section 7).

**Cautions.** (1) The embedder runs on the Mac; a daemon in a pod cannot reach it unless it is exposed, and the estimate needs it at estimate time. (2) The ridge has 2,560 weights, and they are trained on 1,364 beads; a monthly retrain should check that the correlation holds on the following weeks. (3) The result is on a four-day holdout. (4) The training text must be cut at the same place the estimate cuts it: the sections as they stand at dispatch, not after rework notes.

## 7. The operator's scale and the split rule

The operator decided a point is polecat-minutes rounded to the nearest Fibonacci number, with edges at the arithmetic midpoints (1.5, 2.5, 4, 6.5, 10.5, 17, 27.5, 44.5 minutes), and that a bead estimated over 21 points is split before dispatch. Against the span-era data:

| Points | 1 | 2 | 3 | 5 | 8 | 13 | 21 | 34 | 55 | 89 |
|---|---|---|---|---|---|---|---|---|---|---|
| Share of beads, Haiku | 0.2% | 3.5% | 19.4% | 23.7% | 20.6% | 12.6% | 9.5% | 7.2% | 2.2% | 1.1% |
| Share of beads, DeepSeek | 0.3% | 2.1% | 10.1% | 22.8% | 23.3% | 17.8% | 10.5% | 7.3% | 3.9% | 1.8% |

The scale spreads the work well; points 3 to 21 hold about 85% of beads. The beads that actually take more than 21 points (over 27.5 minutes) are 11.8% of beads (Haiku 10.5%, DeepSeek 13.1%) and carry 51% of polecat time.

**The split rule cannot work as worded.** A regression averages toward the middle, so no estimate is a long one: in the 371 common beads, the blends' largest estimate is 20 to 23 minutes, and none is above 27.5. Of the same 371 beads, 29 (7.8%) actually ran over 27.5 minutes. Treating a lower estimate as the split trigger:

| Blend | Trigger: estimated minutes at least | Beads flagged | Precision (flagged beads that ran over 27.5) | Recall (long beads caught) |
|---|---|---|---|---|
| shape + `fits_twenty_minutes` + embedding | 11 | 23% | 0.19 | 0.55 |
| | 13 | 11% | 0.31 | 0.41 |
| | 15 (the operator's trigger) | 4% | 0.40 | 0.21 |
| shape + `fits_twenty_minutes` | 13 | 15% | 0.18 | 0.34 |
| | 15 | 8% | 0.25 | 0.24 |

At the best trigger shown, 15 estimated minutes, four beads in ten that are flagged run long and four in five long beads are missed. Splitting a bead is not free (an overseer or bird must write the pieces). I recommend the rule be a flag for review, not an automatic split, and that the operator decide after the live recompute whether the review load is worth the catch.

## 8. What internal/metrics gave and what it lacked

I did not use the Go tables. They fit only in part:

- `internal/metrics/landings.go` reads the landing ledger into a table with a time-to-land column (created to landed) but has no `polecat_secs`, `polecat_attempts`, lower-bound or `agent` column. Those are the columns this measure needs. A bead in the epic should add them.
- There is no loader for the Jev dispatch log, so the Jev answers could not join the table without new code.
- hm bead text and embeddings are outside the metrics package; the analogy estimator needs a call into hm or its database.
- `backtest.go` and `shape_outcome.go` serve the weekly and rejection analyses; a rank-correlation backtest by seat class with a date holdout is not there.

I ran the analysis in Python as scratch (it is hand-run, off any town path). Nothing here should be copied as production code.

## Not verified

- The embedding ridge: the fresh embeddings were made with ollama at the time of the run; hm's index was being refreshed at the same time. The Constraints and Acceptance text is taken from the hm copy of each bead cut before its first note, which for a bead amended after dispatch can differ from the text at dispatch.
- A decoding slip in my first run left a stray backslash before each newline in the hm text; I fixed it and re-ran every estimator, and the numbers above are the re-run (they moved by at most 0.02).

- That the landing-record minutes include idle waits the usage minutes omit. The 1.4 ratio is measured on 20 beads; the reason is my reading.
- Why the usage scan stopped on 10-29.
- That hm text cut at the first note or comment equals the text at dispatch. A bead amended after dispatch (a SPEC NOTE rework) can differ. For Jev, 14% of its rows were logged after the bead landed, but the battery reads the bead's sections, which can also have been amended.
- Dependency count, an epic feature, is not in the hm text and was not tested. The `risk_paths` of the landing record were left out as post-landing.
- The hm and shape test sets are four days (433 beads); the date holdout is a single cut at 11-03, not a rolling origin.
- I did not re-run on the Mac's copy of the ledgers, and I did not read the 1,528 beads with no minutes.

## Reproduce

The harness is not in the repository. The Go retrain step in the plan (SP16) re-derives the weights, so the Python is the seed and the evidence, not a dependency. Every model above was fitted on beads landed before 11-03 and scored on later ones; the initial weights file in `docs/plans/` was refitted on all 1,797 beads including the holdout.


Scratch files under `~/gt/.runtime/eagle/sp/`: `build.py` (joins the ledgers, reviews, usage records and Jev log into `beads.csv`), `est.py` (the shape, hm and Jev estimators and the backtest), `est2.py` (the embedding ridge), `final.py` (the blends, bucket tables and split sweep), `gen.py` (the initial weights file), `hm_beads.tsv` (the hm export; 343 MB), `pod/` (the copied ledgers), `backtest_hm_testset.csv`. The pod ledgers were copied on 2026-11-11 about 12:30.
