> Status: historical (2026-10). Merged in: gt-sxg5u. Not maintained. The replay was replaced by `gt jev attention` plus `gt jev scorecard --battery attention` (gt-3ey5j).

# Jev advisory triage: implementation plan

Design: docs/plans/2026-10-19-jev-attention-triage-design.md (bead gt-sxg5u).

Task beads were held with `needs-human` until approval, then released in dependency order.

## Tasks

| Bead | Task | Depends on |
|---|---|---|
| gt-iek4e | Jev client: Choice call, timeout, 429, usage capture | none |
| gt-bwdp0 | Triage prompt and fail-safe mapping (pure functions) | none |
| gt-qml0l | Attention queue reader (state, events with rotation, acks; read-only) | none |
| gt-s1xj6 | Ack history `acks-history.jsonl` (tier-2 ground truth; design section "Measuring the pilot") | none (dispatched 11:46, furiosa) |
| gt-vbkfy | Jev replay report: tier-2 and tier-1 labels, precision, recall, calibration, spend | gt-iek4e, gt-bwdp0, gt-qml0l, gt-s1xj6 |
| gt-pmfyq | Live triage job behind `attention.jev_triage.enabled` (default off) | gt-iek4e, gt-bwdp0, gt-qml0l |
| gt-irbdc | `gt attention` jev column, display only | gt-pmfyq |

Three tasks start in parallel. The replay report and the live job share the client, the mapping and the reader, so they start after those land. The display column comes last, because it reads the file the live job writes.

## Order of work

1. **Land the three foundations** (gt-iek4e, gt-bwdp0, gt-qml0l). Each lands through `gt done`, one at a time when they touch the same package.
2. **Land the replay report** (gt-vbkfy). Operator step: Sloan supplies the API key (`~/.config/gt/jev.env`), and the pilot runs the replay with the $5 cap. The pilot report goes to Sloan before anything else moves.
3. **Land the live job** (gt-pmfyq) with the flag off. Turning it on is Sloan's decision after the pilot report.
4. **Land the display column** (gt-irbdc). It stays a dash until the live job is on.

## Gates and checks

- Each bead's gate is `make gate` plus its own test target, as written on the bead.
- Every bead's acceptance list is checked on `origin/main` after landing, not on the crew branch.
- The attention queue files must be byte-identical before and after any run. gt-pmfyq and gt-irbdc each carry that test.
- The pilot report must show real token counts and the spend against the cap, and must say that the ground truth is a proxy.

## Operator steps (not polecat work)

- Sloan approves this plan. Then the holds come off, in the order above.
- Sloan provides the Jev API key. It goes in `~/.config/gt/jev.env`. Nobody prints it or puts it in a commit. Check: the file is mode 600, and `git grep` for the key returns nothing before any commit.
- Sloan reviews the pilot report and decides whether the live job goes on.
- The kickstart after turning the flag on is done by the overseer, on a quiet town.

## Out of scope

The other three uses (deep-review dedupe, P3 severity, risk-diff second opinion) and the Jev spend dashboard panel (bead gt-z29gt) each get their own design after this pilot.
