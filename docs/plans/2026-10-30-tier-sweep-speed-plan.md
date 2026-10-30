> Status: historical (2026-10). Merged in: gt-jzwoz (design and plan; the implementation beads are gt-ic4sf, gt-dwlbj, gt-xk9my, gt-0rtgc and the rest listed in the plan). Not maintained.

# Tier sweep speed: implementation plan

> Author: dunlin, 2026-10-30. Design: `2026-10-30-tier-sweep-speed-design.md`. Evidence: `~/gt/.runtime/overseer/birds/tier-sweep-findings.md`.
> Every task is ONE Gas Town bead (about 20 minutes of one polecat, one package, acceptance boxes with a narrow `go test -run`), filed and linted; the bead is the task text, this plan is the order and the reasons. Go, not shell. No box runs `make gate` or measures the live town; live measurements are listed under "Owed" and belong to the overseer.

**Goal:** cut the gastown sweep at least in half (quiet 11-13 min to 5-6, loaded p90 29 min to under 15), the devops worst case from 27 min to under 5 and its typical sweep under 2, and take the sweep off the daemon's host.

**Order (overseer decision 2026-10-30):** measure first, then the container move, then package-level speed-ups; the cheap host-side steps do not wait for the container.

**Scope split (not a placeholder):** the shell tier's own deletions and ports belong to the shell-tier-to-Go program (gt-tpg0p, plan 2026-10-28-shell-tier-to-go-plan.md); this plan only orders them by wall time. The devops sweep's move onto the Go sweep and the wizard ports beyond DV1 are shaped one at a time as their predecessors land (dv-fec7's rule), not pre-filed.

## Beads

Dependencies are real edges (`bd dep`); the dispatcher slots a bead when its parents have landed. Held beads are deferred with `needs-human`: the overseer releases them.

### Wave 1: measure and clean (internal/tiersweep chain, then the daemon)

| Order | Bead | Package | What | Depends on |
|---|---|---|---|---|
| 1 | gt-ic4sf (S1) | internal/tiersweep | per-package wall in `timings.jsonl`, and the persistent TIER_SWEEP_TIMINGS file | none |
| 2 | gt-dwlbj (S5) | internal/tiersweep | TIER_SWEEP_NICE knob for the nice-0 A/B (default unchanged) | S1 |
| 3 | gt-xk9my (S2) | internal/tiersweep | a GREEN sweep removes its own log dir; `PruneCandidates`/`Prune` of old `tier-sweep.*` | S5 |
| 4 | gt-g6xz7 (C1) | cmd/tier-sweep | `tier-sweep prune` lists, `--apply` deletes (the 9120 directories, operator-approved list) | S2 |
| 5 | gt-njgu2 (L1) | internal/land | `StepResult.SlotWait` | none |
| 6 | gt-sy14g (D1) | internal/daemon | log and record the slot wait of the integration stage | L1 |

### Wave 2: speed on the host (the cheap steps)

| Order | Bead | Package | What | Depends on |
|---|---|---|---|---|
| 7 | gt-0rtgc (S7) | internal/tiersweep | N bounded workers (TIER_SWEEP_JOBS, default 1), longest first, `serial.txt` alone | S2 |
| 8 | gt-loqew (S6) | internal/tiersweep | TIER_SWEEP_RESUME: a package that passed at this sha is not rerun | S7 |
| 9 | gt-n3hql (D2) | internal/daemon | pass the timings and resume files to Go-command stages; the 15-minute restart bound is NOT raised | D1, S6, S1 |
| 10 | gt-difbn (S3) | internal/tiersweep | pure `go test -json` to plain text plus per-test times | S6 |
| 11 | gt-juo5e (S4) | internal/tiersweep | integration runs with `-json`; slow tests (5 s and up) into `timings.jsonl` | S3 |
| 12 | gt-2daks (S8) | internal/tiersweep | build gt once per sweep, export TIER_SWEEP_GT_BIN | S4 |
| 13 | gt-7lhic (C2) | internal/cmd | `buildGT` uses the prebuilt gt (narrow `-run`) | S8 |
| 14 | gt-j68dz (S9) | internal/tiersweep | pure partition of test names into balanced shards | S8 |
| 15 | gt-rw118 (S10) | internal/tiersweep | run `./internal/cmd` as 3 shards of one binary; coverage proved (listed equals run); ships disabled until a quiet live run | S9 |

The decision on the upgrade-restart interplay (28 waits, one cut sweep): keep the bound (it is a budget; never raised), make a cut cost one package (S6 + D2), and let the runner (wave 3) remove the cut entirely, since a detached runner survives a restart.

### Wave 3: the separate runner (design for the k8s driver)

| Order | Bead | Package | What | Depends on |
|---|---|---|---|---|
| a | gt-74ix9 (R1) | internal/sweeprunner | Spec, Backend interface, Docker backend rendered as argv (golden) | none |
| b | gt-7zy61 (R2) | internal/sweeprunner | Kubernetes backend: Job manifest, requests = limits, `gt-ci`, dind sidecar only privileged | R1 |
| c | gt-u00wo (R3) | internal/sweeprunner | `result.json` write/read/convert to the daemon's tier results | R2 |
| d | gt-dibnm (R4) | cmd/sweep-run | the container entrypoint: clone sha, run the sweep in process, write the result | R3 |
| e | gt-aq0cw (R6) | internal/testpolicy | `sweep-host.txt` (shrink-only) and the guard that every integration package is classified | none |
| f | gt-q7wkn (R5) | deploy/polecat, Makefile | HELD: dolt and `sweep-run` into the polecat image | R4 |
| g | gt-z4ppb (R7) | internal/config, internal/daemon | HELD: `daemon.patrols.tier_sweep.runner`; the daemon launches and reads; a missing result is RED | R1-R4, R6, D1 |

### Wave 4: devops

| Bead | Package | What |
|---|---|---|
| dv-aq2m (DV1) | devops internal/auth/runrules | the run-rule table check in Go with a table test (extraction 1 of 2) |

Queued, shaped when their predecessor lands (not filed): DV2 the wizard calls the Go check and 29 harness cases collapse to two; the same extraction for `auth-broker-credentials` (23% of the wizard unit tier), then `install-deploy-runner` (13%), the add-app family in `ops/wizards` (8%), `platform-cleanup-compute` deleted (tranche 1, 5%); host-rehearsal ports in wall order: gcp-init-stacks, gcp-bootstrap, gke-reap, publish-rule; the devops sweep onto the Go sweep (needs the `-json` host-rehearsal run, its race and shell lists, and the same cleanup).

## Measuring before and after (the same sweep)

1. Baseline (before any bead): the numbers in the findings note (61 gastown sweeps, 13 devops).
2. After wave 1: S1 gives per-package wall on every sweep; the overseer runs one sweep with TIER_SWEEP_NICE=0 and one unset on the same sha (A/B) and records the two timings files in a bead comment.
3. After wave 2: three runs at TIER_SWEEP_JOBS=4 against three serial runs, same sha, quiet (load1 under 10) and loaded; packages that fail only in parallel go to `serial.txt` with the output attached. Then `./internal/cmd 3` enabled in `shard.txt`, one quiet sweep, wall compared with the 196 s median.
4. After wave 3: one sweep through the runner against the host sweep for the same sha; the runner's wall must not move with host load (run it once while a gate suite runs).
5. Daemon coupling: rerun `~/gt/.runtime/dunlin/lock/contention.py` (heartbeat gap and sling time, inside versus outside a sweep) after 48 hours with the runner on; the targets are +3% and +10%.

## Owed (live, not boxes)

- Overseer: the A/B above; the concurrency runs; enabling shards; the first full Linux run of all 66 packages in the runner (lists failures; R6's manifest takes the host-bound ones); the prune list review by the operator and `--apply`.
- Operator: the design's four decisions (section 7).
- Seagull (daemon-container plan): C2 reframed as the runner's engine; the daemon image needs a Docker client or API access for the launch seam; the sweep leaves the daemon container.

## Not planned

Raising any timeout, budget or ratchet; skipping or retrying a test; a name filter that can skip tests silently; a per-package skip trigger for all 66 packages until the timings show the tail is worth the cache-honesty work.
