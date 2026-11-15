> Status: historical (2026-11). Merged in: gt-1m0kb. Not maintained.

# Polecats with no bead access: implementation plan

Plan for gt-1m0kb. The design and the thirteen accepted recommendations are in the
`~/.claude` repo, `docs/specs/2026-11-12-polecat-no-bead-access-design.md` (section 8,
accepted by the operator in grilling round 3, 2026-11-13). The call census is
`docs/design/polecat-no-bead-access-census.md` (landed 2026-11-14). The operator's ruling of
2026-11-14: a polecat calls no outside service it does not need, and a polecat calling its
own review is debt to purge.

Goal: a sandbox seat holds no route to the data plane (Dolt, port 3307) and runs no `bd`
or `gt` call that reads or writes a bead. The task arrives as a file at spawn, the result
leaves as an outbox the host validates and applies. Tmux seats stay as they are.

## Global constraints

- Approach A of the design: `TASK.md` in, outbox out; sandbox seats only.
- One bead per task below: under 8 points, one deliverable, boxes a polecat can tick alone.
  The split trigger is never raised; a bead that estimates over it is split by file group.
- Pure Go with table tests and injected seams (no network, no sleeps, no wall clock, no
  `t.Setenv`); read `~/.claude/docs/standards/test-engineering.md` before writing a test.
- No new shell or Python logic, no new `GT_` environment reads.
- The host validates the outbox as untrusted input and writes under the seat's actor name.

## Phase 0: remove what the seat calls today and should not (runs first, in parallel)

These shrink the call list the later phases must replace, so they go before them.

| Task | Bead | Effect on the census |
|---|---|---|
| Local review round removal, four beads | gt-dpl81b, gt-6qb84r, gt-i9rw9d, gt-755l63 | removes `gt review-preview` and the `gt done` local round gate, so file-mode `gt done` never has to port them |
| Step wisp removal, chain filed from the trace | gt-fcx123 (trace) then its removal beads | removes `gt mol step done`, `gt prime --step` and the molecule close inside `gt done` from the seat |
| Seat-end hooks that read the agent bead | none: host side already | the stop hook's agent bead read stays host side |

## Phase 1: the task file (the seat still has Dolt, nothing changes for it)

| # | Task | Files | Depends |
|---|---|---|---|
| T1 | A pure assembler: bead fields in, `TASK.md` text out (title, description, acceptance boxes with state, notes tail, open comments, the formula steps as a checklist) with golden tests | create a taskfile package | none |
| T2 | A test that `TASK.md` for a fixture bead carries every fact `gt prime` prints for the same bead | taskfile test, `internal/cmd/prime_payload.go` read-only | T1 |
| T3 | The daemon writes `TASK.md` into the sandbox at spawn, read-only, in a directory the host excludes through `.git/info/exclude` before the seat starts | `internal/session/backend_openshell.go` and its ops file | T1 |
| T4 | Depth-one snapshot: the beads the task text names are rendered read-only into the task directory | taskfile package | T1 |
| T5 | Rework: a re-slung seat gets `TASK.md` rebuilt with the om, gate and overseer findings in it, and the rejection-unchanged fact (`done_rejection_unchanged.go:284`) written into it, because `gt done` cannot ask the bead mid-run | daemon sling path | T3 |

## Phase 2: the outbox and the host apply

| # | Task | Files | Depends |
|---|---|---|---|
| O1 | Outbox schema and validator: `result.json` (status, ticked boxes, findings, escalation request) and `notes.md`; size cap, own bead id only, a fixed list of actions, no label, assignee or close of other beads | create an outbox package | none |
| O2 | Host apply: from a validated outbox write the notes, ticks, comments, status and findings under the seat's actor; no swallowed errors (`internal/done/done.go:2030` ignores its `bd.Update` error today) | outbox package, beads client | O1 |
| O3 | Seat-end copy-out of the outbox before the sandbox is deleted, with the transcript copy; a failed copy keeps the sandbox | `internal/cmd/sandbox_seat_*`, session package | O1 |
| O4 | HELP: the escalation request in the outbox is read at seat end and on the patrol tick (about 10 minutes) and creates the escalation bead as the seat | daemon patrol | O2 |
| O5 | The landing block: the host writes the READY TO LAND block from the applied outbox after the seat ends | landing code | O2 |

## Phase 3: the file-mode client (behind a profile key, Dolt still open)

| # | Task | Files | Depends |
|---|---|---|---|
| F1 | The sandbox profile key `bead_access: files|dolt` (default `dolt`), plumbed to the seat environment as a profile value, not a new `GT_` read | config and session | none |
| F2 | File-mode `gt prime` and `gt hook` print from `TASK.md` | `internal/cmd/prime.go`, `hook.go` | T3, F1 |
| F3 | File-mode `gt bead note`, `comment` and `update --acceptance|--title` append to the outbox on every call (write-through) | `internal/cmd/bead.go` | O1, F1 |
| F4 | File-mode `gt escalate` writes the escalation request to the outbox | `internal/cmd/escalate_impl.go` | O1, F1 |
| F5 | File-mode `gt done`: refuses on unticked boxes from the seat's file state, pushes the branch, writes `result.json`, exits | `internal/cmd/done.go`, `internal/done` | O1, F3, Phase 0 |
| F6 | Hooks: drop `gt mail check --inject` and `gt mail inbox` from the seat in file mode; the stop hook reads nothing from the seat | `internal/hooks/config.go` | F1 |
| F7 | Formula text: drop the connectivity probe and the mail step; step text computed from live bead state is found and replaced (the design lists this as not verified) | the polecat formula | F2 |

## Phase 4: proof, flip and cleanup

| # | Task | Depends |
|---|---|---|
| P1 | The live proof on one seat (sbx-haiku-1) with a small gastown bead: it lands, its notes and ticks are on the bead, a forced HELP creates an escalation, the profile's policy has no 3307 rule and `bd show` in the sandbox fails closed. An overseer step recorded on its bead. | all of Phase 3 |
| P2 | Flip the default to `files` for every sandbox profile; delete the `dolt:` block in `deploy/k8s/policy/kind.yaml` and the Docker policy and the in-cluster 3307 Service; keep a one-line rollback for a week | P1 |
| P3 | Delete the Dolt path from `gt prime` and `gt done` in sandbox mode and the Dolt-down preflight probe; close the per-polecat Dolt users ticket (map claude-q3f) as moot | P2 plus a week |

## Open points the first beads must settle

- The submit-gate reads the census says an outbox cannot answer (rejection-unchanged, the acceptance checks inside `gt done`): T5 puts the rejection fact in `TASK.md` at spawn; the acceptance check runs from the seat's file copy. A bead edited by an operator mid-run is not seen by the seat until it ends; the host's apply re-reads at seat end.
- Inbound mail and the `gt tap guard` family: guards stay in the seat (they make no bead call); inbound mail is dropped for sandbox seats (the directives already forbid it).
- `gt-wrap` and the file-argument scanner in front of `bd` and `gt` in the sandbox: F2 to F4 check whether file mode needs allow rules for the task and outbox directories.

## Order and parallelism

Phase 0 runs now, in parallel with T1 and O1 (both pure packages with no dependency).
T2 to T5 follow T1; O2 to O5 follow O1; the two streams meet at F2 to F5. Beads are filed
in waves: T1 and O1 now, each later wave when its predecessors have landed, so every bead
names files verified against landed code.

## Reading the result

Success is the proof in P1 plus the policy diff in P2: a sandbox that holds routes to the
model endpoint and Forgejo and none to the data plane.
