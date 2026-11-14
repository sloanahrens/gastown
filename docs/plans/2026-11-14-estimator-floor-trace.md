> Status: historical (2026-11). Merged in: gt-i1aoho. Not maintained.

# The estimator floor: why small beads estimate 11 to 13 minutes

Traced 2026-11-14 against origin/main `5efde70c`, with the weights the binary
embeds: `internal/storypoints/weights.json`, version 1, source `embedded v1`.
On that day four shaped beads of 2 to 4 acceptance boxes and 2.5 to 3.2 KB of
text (gt-l37xdn, gt-8heuxp, gt-yr3e14, gt-emvv9r) and three five-command
task-map beads (gt-j669b4.14, .15, .16) all carried `sp:13` with `sp-minutes`
between 11.0 and 13.0, so the split route held them again after every manual
split. This document says where those minutes come from, with the numbers.

## 1. What the estimate computes

`estimate.Estimate` (internal/storypoints/estimate/estimate.go) builds three
inputs and hands them to `storypoints.Blend`.

- **shape**, always built. `storypoints.Features` (internal/storypoints/shape.go)
  counts twelve numbers from the bead text; `Weights.ShapeMinutes`
  (internal/storypoints/model.go) turns them into minutes as
  `exp(intercept + Σ coef_i·(x_i − mean_i)/scale_i)` with intercept `2.16992`.
  The twelve means, scales and coefficients are the `shape` block of
  weights.json.
- **embed**, built when an embedder is configured. `embedInput` sends the
  Constraints and Acceptance sections (`SectionText`, internal/storypoints/embed.go)
  to an ollama embedder, and `Weights.EmbeddingMinutes` reads the 2,560-number
  vector as `exp(intercept + dot(coef, vector))` with intercept `1.485249`.
- **jev**, built when the Jev battery opens. `jevInput` asks the dispatch
  battery for `fits_twenty_minutes`, a probability, and `Weights.JevMinutes`
  (internal/storypoints/blend.go) maps it to minutes by linear interpolation
  over 46 knots from `71.697` at p ≤ 0.05 to `1.942` at p ≥ 0.86. The knot at
  p = 0.5 is `10.941`.

`storypoints.Blend` (internal/storypoints/blend.go) averages the available
inputs **on the log scale with equal weight**: `exp(Σ ln m_i / n)`. Two inputs
make one `shape-jev` or `shape-embed` estimate, three make rung `all`.
`storypoints.FromMinutes` (internal/storypoints/points.go) rounds the minutes to
a Fibonacci point with edges at 10.5 and 17, so a time over 10.5 and up to 17
minutes prints `sp:13`.

The dispatcher's estimate is wired in `specEstimateDeps`
(internal/cmd/spec_estimate.go); the split route reads its minutes in
`specStoryPointStep` (internal/cmd/spec_storypoints.go) and compares them with
`patrols.spec_dispatch.story_points.split_over_minutes`
(`SpecDispatchConfig.SplitOverMinutes`, internal/config/daemon_config.go),
which was 11 on this town (each `SPLIT ROUTE v1` note names it).

## 2. gt-j669b4.14, from its text to 11.8 minutes

The bead's current sections hash to `c4938024…c32c`, the hash in its
22:04:41 `SP ESTIMATE v1 sp:13 rung:all` note (minutes 11.8). The estimate read
exactly today's text, so its shape component is the one those features produce:

| feature | value | training mean | std from mean | log-minutes term |
|---|---|---|---|---|
| boxes | 4 | 2.919 | +0.72 | +0.075 |
| dlen | 8.387 (=4,387 runes) | 7.826 | +1.23 | +0.225 |
| tests | 17 | 7.037 | +1.51 | +0.116 |
| nlab | 5 | 1.796 | +2.70 | +0.052 |
| pkgs | 4 | 2.356 | +0.74 | +0.032 |
| rig_gt | 1 | 0.834 | +0.45 | +0.019 |
| other six | | | | −0.015 net |

The intercept alone is `exp(2.16992) = 8.76` minutes; the intercept plus these
terms is `2.675` log minutes, so **shape = 14.51 minutes** — `sp:13` on its own,
above the 11-minute trigger before any other input is read.

The note says the blend was 11.8 on rung `all`, so embed and jev were both
present:

```
(14.51 · embed · jev)^(1/3) = 11.8  ⇒  embed · jev = 11.8³ / 14.51 = 113.2
```

The geometric mean of the two model inputs is therefore `10.64`: embed and jev
each landed near 10.6 minutes. No input reported the bead as small; the shape
input was the largest of the three.

The same arithmetic on all five beads, whose notes match their current section
hashes. Running the estimator again on each (`gt spec estimate --force
--dry-run --json <bead>`, 2026-11-14) returned the minutes at one decimal —
14.5, 9.9, 9.7, 10.8, 10.2 — so each two-decimal figure below is what that
estimate read:

| bead | shape minutes | note minutes | embed · jev | pair mean |
|---|---|---|---|---|
| gt-j669b4.14 | 14.51 | 11.8 | 113 | 10.6 |
| gt-l37xdn | 9.87 | 12.4 | 193 | 13.9 |
| gt-8heuxp | 9.65 | 13.0 | 228 | 15.1 |
| gt-yr3e14 | 10.83 | 12.7 | 189 | 13.8 |
| gt-emvv9r | 10.16 | 11.0 | 131 | 11.4 |

Across all five, the shape input lies between 9.65 and 14.51 minutes and the
remaining pair's geometric mean between 10.6 and 15.1. That is the floor: three
independent predictors of the same prose, each calibrated to the middle of the
landed distribution, and an equal-weight mean of three middling numbers is a
middling number.

## 3. What sets the floor

The shape input does, for three reasons.

1. It is the only input every estimate has, and it alone is 14.51 minutes for
   this bead — 32% over the trigger. `Blend` refuses an estimate without it
   (`ErrNoShape`).
2. Its base is the training target's centre. `exp(intercept) = 8.76` is what
   the model predicts for a bead at every training mean, because the feature
   terms vanish there (`shapeFit.logMinutes`, internal/storypoints/train.go).
   The ridge leaves the intercept unpenalized, setting it to the mean of the
   training log-minutes less the coefficients on the feature means
   (`ridgeFit.solve`, internal/storypoints/ridge.go), and `shapeLearner`
   standardizes the features before the fit, so that mean is zero and the
   intercept is the training target's mean. `exp(2.16992)` is therefore the
   fit's geometric mean of the minutes it was trained on — landed beads
   2026-10-26..11-06, n=1797 (weights.json, `trained_on`). The landing record's
   medians for that era are lower: 7.0 minutes on the Haiku seat and 8.6 on the
   DeepSeek seat, with a pooled training-period median of 7.4 and p70 of 13.0
   ([retrospective](2026-11-11-story-points-retrospective.md) sections 1 and 2),
   the order a geometric mean takes above the median of a right-skewed
   distribution. A prose-heavy bead is lifted from that centre, never lowered.
3. The two features that lift it measure prose, not work. `dlen` is
   `ln(1 + runes)` and multiplies the estimate by `exp(0.225) = 1.25` at this
   bead's 4,387 characters; `tests` counts matches of `\btest` and
   multiplies it by `exp(0.116) = 1.12` at 17 of them, because every gate line
   in the template names a `go test` command. Nothing in the twelve features
   counts the five one-line commands the bead actually asks for.

The jev input has the same shape of floor: at p = 0.5, the battery saying it is
unsure, it returns 10.941 — a hair under the 11-minute trigger. It can only report
a small bead by being confident (p ≥ 0.86 gives 1.942), and on this bead it was
not. So the floor is not a hard clamp: had jev returned 1.94, the
blend would have been `(14.51 · 10.6 · 1.94)^(1/3) = 6.7` — `sp:5`. The
estimator produced 11.8 because none of the three inputs saw a small bead, not
because one of them caps the answer.

The second half of the cause is the trigger's placement. The split trigger was
11 minutes, against a pooled training-period median of 7.4 and p70 of 13.0
(retrospective section 2); on actual minutes, 35.6% of span-era beads run past
11 (the sum of that section's four columns above its 11-minute bound). Section
7 sets the trigger on the estimate instead, as the split rule does: at 11
estimated minutes its three-way blend flags 23% of beads, and 19% of the
flagged beads run over 27.5 minutes. The best trigger shown there, 15 estimated
minutes, flags 4% at precision 0.40 — four beads that run long for every six
that do not (section 7's table; the summary's point 5 states the ratio).

## 4. Four smaller mechanisms next to the floor

- **The estimate feeds its own labels back in.** `EstimateText`
  (internal/storypoints/shape.go) writes the bead's label list into the first
  line, so the `sp:`, `sp-rung:` and `sp-minutes:` labels a previous estimate
  wrote are text for the next one: `nlab` counts them and `dlen` measures them.
  Removing them from gt-j669b4.14's first line and repeating section 1's
  arithmetic lowers its shape input from 14.51 to 13.78 minutes, 5%.
  `estimate.SectionHash` covers only the Goal, Constraints, Acceptance and Size
  sections, so a bead whose description is edited is re-estimated on a title
  line that has grown by every label written since.
- **A held bead keeps being estimated.** `specEstimateAheadPick`
  (internal/cmd/spec_estimate_ahead.go) does not exclude a bead carrying the
  split label, so editing gt-j669b4.14's text produced a fresh note at 22:04.
- **`boxes` counts acceptance checkboxes, and the template asks for one per
  gate**, which is why a five-command bead scores 4 boxes against a training
  mean of 2.9.
- **The Size section is text only.** gt-j669b4.14's "about 33 minutes" is
  measured by `dlen` like every other character; its content changes nothing.

## 5. What I could not determine

- **The embed and jev components of any past estimate.** There was no estimate
  log to read: on 2026-11-14 the town's `.runtime` held no `storypoints`
  directory, so `estimates.jsonl` (the path `estlog.Path` names,
  `<townRoot>/.runtime/storypoints/estimates.jsonl`) did not exist.
  The forced estimate run in section 2 (`gt spec estimate --force --dry-run
  --json <bead>`) skipped both — the embedder URL `host.docker.internal:11434`
  does not resolve from the sandbox, the files the battery reads hold no
  `TYPESAFE_API_KEY` line, and hm is not installed — and returned rung `shape`
  for every bead, the one-decimal minutes section 2 quotes. That is how the
  shape components above were confirmed. The pair product in section 2 is
  arithmetic on the recorded blend, not a measurement of either input.
- **Why the log is missing.** Either the daemon's binary predates the append
  (gt-a86xuu) or the append failed; the note's `LogError` is reported on the
  dispatcher tick, not on the bead, and I did not read the daemon's log.
- **What the battery answered and how well its knots are calibrated.** The
  knots are the checked-in ones and `gt metrics storypoints-train` does not
  refit them (docs/reference.md, Retrain), so a weights fix cannot move them.
- **How long gt-j669b4.14 really takes.** It never landed, and its own Size
  line says 33 minutes.

## 6. Fixes, cheapest first

None of these changes `split_over_minutes`, in code or in the town's settings:
the operator's rule is that the trigger is never raised to release a bead.

**A. A size term — about 3 points.** Add a fourth input to
`estimate.Estimate`: minutes from the work the bead's own text enumerates, for
example `2 + 1.5 × (commands or paths listed in a fenced list)` = 9.5 for
gt-j669b4.14's five commands, with `boxes` as a fallback. It needs no retrain
and no service. An equal-weight fourth term is not enough on its own — it moves
11.8 to 11.2 — so the term must act as a cap (`min(blend, size)`, 9.5 → `sp:8`)
or replace `dlen`'s weight, and the bead should say which. Touches
internal/storypoints/estimate/estimate.go and internal/storypoints/blend.go with
table tests per input combination; the note's `inputs:` line and the rung names
gain the term.

**B. Model weights — about 5 points.** Refit the shape ridge through
`gt metrics storypoints-train` (internal/storypoints/train.go) with `dlen` and
`tests` either dropped or fitted against landed times, and adopt the proposed
file when its holdout clears 0.4. It needs the thing this trace found missing: a
corpus of landed small beads to learn from, and the estimate log to date the
features. The weights-only step first, with no retrain, is a clip of `dlen`'s
standardized value at ±1: gt-j669b4.14 falls from 14.51 to 13.9. Dropping
`dlen`'s term outright takes it to 11.6 — still over the trigger — and throws
away the feature that tracks how much there is to read without supplying one
that tracks how much there is to do. That is why the retrain, which can fit a
term that points the other way, is the weights fix and the clip is not.

**C. Neighbour weighting — about 7 points, and last.** Today
`nearest.Nearest` (internal/storypoints/nearest/nearest.go) only names the
nearest past beads; its own doc says hm does not set the number. Weighting them
into the estimate means reading each neighbour's measured minutes (`sp-actual:`)
and fitting a similarity-to-minutes term. The retrospective measured hm analogy
alone at rank correlation 0.27 to 0.29, the weakest input it tested, so this
buys least per point spent.
