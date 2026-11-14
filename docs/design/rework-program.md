# Cut the polecat time lost to review rework and seat restarts

Status: approved by the operator, 2026-11-13 (grilling round 1, Q1 to Q8, all accepted). Program bead: gt-g3axjd.

Where each decision is recorded in this file: Q1 under "Goal and measure"; Q2 lever 1; Q3 lever 7; Q4 lever 3; Q5 lever 4; Q6 lever 5; Q7 lever 2; Q8 lever 6. The full text of the grilling round is on program bead gt-g3axjd, which is not in this repository.

## Evidence (landing record 2026-10-26 to 11-11: 2,454 landed beads, 743 polecat-hours)

- Beads with two or more om review rounds are 31% of beads and about two thirds of polecat time (median 6.2 minutes at one round, 17.8 at two, 32.0 at three).
- Beads that needed two or more seats are 29% of beads and 55% of time. 71 beads with one review round but two seats took a median 35 minutes against 5.9: pure restart cost.
- Beads touching three or more risk paths (internal/land/riskpaths.txt) are 11% of beads and 18% of polecat time. Their median polecat minutes are three times the median of all landed beads.
- Estimator: a nearest-neighbour on the Constraints and Acceptance section embeddings gives rank correlation 0.44 to 0.48 (the first test used the wrong text and a stale pool: 0.27 to 0.29); ridge 0.57; blend 0.67. Neighbours' rework share predicts two or more rounds with AUC 0.70, shape alone 0.73, both 0.74; a long bead (over 27.5 minutes) is only 0.57 to 0.63.
- The polecat directive reaches seats (the 2026-11-13 seat prompts carry it) but has grown to 15.4 KB from 10.0 KB (gt-pccx7). The weekly lessons pass (docs/standards/om-lessons-loop.md) is run by a bird; its ledger was last written 2026-11-07.

## Goal and measure (decision Q1)

Halve the share of beads needing two or more review rounds (31% to about 15%) and cut mean polecat minutes per landed bead by a third, judged weekly on equal windows from the landing record. One daemon step writes the two numbers every week (lever 6).

## Levers, in the order they are built

1. **Seats push as they go (Q2).** The polecat formula tells a seat to commit and `git push origin HEAD` its own branch after each commit, so a seat stopped at 30 minutes loses nothing and the next seat continues from the pushed branch. Formula and directive text only; no daemon logic.
2. **Risk-path warning at filing (Q7).** `gt spec lint` warns, never refuses, when a bead names three or more files on the risk list ("one risk path per bead"). Review after two weeks.
3. **The lessons loop runs without a bird (Q4).** A weekly daemon step runs the om-lessons class table (internal/cmd/land_om_lessons.go) and files one shaped, overseer-only directive-edit bead per class with counts, three example beads and the proposed text. The overseer vets; any rule that changes what polecats may do needs the operator's yes. No Claude calls.
4. **Directive size budget (Q5).** The target is 12 KB (12,288 bytes) for the injected polecat directive. The test does not enforce the target yet: `TestDirectiveCeiling` (internal/cmd/directive_size_test.go) holds the rendered directive to the committed ceiling in internal/cmd/testdata/directive_ceiling.txt, and each cut lowers that ceiling. The committed ceiling is 15,384 bytes, 3,096 bytes over the target, so the budget is already exceeded and every cut is still owed. Order: lever 3 lands first, since the cut pass needs the citation counts its class table gives; the first automatic pass then proposes cuts for rules no finding cited in four weeks, and the ceiling falls with each one.
5. **Rework-risk predictor, estimator v2 (Q6).** Predict minutes and the chance of two or more rounds from shape features plus nearest-neighbour history, validated with a rolling origin, retrained monthly, used as a flag and label at filing only (never a block). Waits on the report fix (gt-kjm3g8, landed). Design bead first, then the validation harness, then the predictor.
6. **The weekly numbers (Q8).** A daemon step writes the program's two numbers and the lever deltas to the dashboard feed.
7. **Local review round (Q3).** `daemon.local_round` gives a seat om's review before it submits. Not before the plan reset on 2026-11-18 (it spends om, which is on the operator's Claude plan); then a one-week trial on the night haiku seats, compared by rounds per bead.

Already built and shipping off: the story-point gates (gt-5d9k2f.18 dispatcher split route, gt-d4uh4f lint gate). They are flags until the predictor can see long beads.

## Out of scope

Changing om's bar or threshold; a hard block from the predictor; replacing the story-point scale.

## How we know a lever worked

Each lever has a before and after window of equal length in the weekly numbers, with bead counts; fewer than about 20 beads in a window is "too few to say". Lever 1 is read from the share of two-seat beads with one round (the 71-bead row).
