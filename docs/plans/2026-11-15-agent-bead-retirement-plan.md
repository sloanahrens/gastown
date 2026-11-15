> Status: proposed (2026-11). Approved by the operator in a grilling on 2026-11-15; nothing here is implemented yet.

# Retiring the agent identity beads: plan

Design: [2026-11-15-agent-bead-retirement-design.md](2026-11-15-agent-bead-retirement-design.md).

Each task is one polecat bead of at most eleven estimated minutes, one deliverable.
Waves are filed as their predecessors land, so a bead never names a function that
does not exist yet. Wave 2 is gated by a measured shadow window, not by a date.

## Global constraints

- Read `~/.claude/docs/standards/test-engineering.md` before writing a test; never
  raise a timeout or budget to land.
- Name every test a bead adds with the prefix `TestAgentBeadRetire`, and give the
  bead the gate `go test <package> -run 'AgentBeadRetire' -count=1`, so the gate
  covers every test the bead adds. A bead that edits an existing test keeps that
  test's own name in its gate as well.
- The intent record's readers fail closed: an unreadable record is held, never
  free. A reader moved onto it keeps that rule. An unreadable hook is never read
  as a re-sling.
- No bead in waves 1 and 2 stops writing the agent bead or changes what is decided.
- A new file named in a bead is written "create X"; no acceptance box names a
  runtime path, `kubectl` or `docker`.

## Wave 1: additive (the intent record learns the facts)

| # | Task | Files | Depends |
|---|---|---|---|
| P1.1 | Inventory by field: a table of every non-test reader and writer of `notification_level`, `role_type`, `active_mr` and `last_source_issue`, in a doc, with a verdict for each (display, decision, derivable) | create `docs/design/agent-bead-field-readers.md` | none |
| P1.2 | The intent record gains `turn_state`, `exit_type`, `cleanup_status` and `last_work_bead`, with setters that clear them when new work is slung and tests | `internal/intent/intent.go` | none |
| P1.3 | `gt done` writes the finished-turn facts to the intent record before it writes the agent bead | `internal/cmd/done_agent_state.go`, `internal/done/done.go` | P1.2 |
| P1.4 | The overrun guard's cleanup-status write also goes to the intent record | `internal/daemon/overrun_guard_stop.go` | P1.2 |
| P1.5 | Create `internal/seatfacts`: `SeatFacts(rig, name)` from the intent record, and a pure `Disagreements` comparator against a `patrolscan.AgentRecord`, with tests | create `internal/seatfacts/seatfacts.go`, `seatfacts_test.go` | P1.2 |
| P1.6 | Shadow compare in `patrolScanHost.AgentRecord`: read both, log each disagreement once per seat and field per tick, still return the bead's values | `internal/daemon/patrol_scan.go` | P1.5, P1.3 |
| P1.7 | Shadow compare for the pool readers: dead-hooked count, seat cap and sandbox seat end log intent `work_bead` against the hook they read | `internal/cmd/pool_seats.go`, `internal/daemon/patrol_scan_seatcap.go`, `internal/daemon/sandbox_seat_end.go` | P1.5 |

P1.1, P1.2 run in parallel; P1.3, P1.4 and P1.5 run in parallel after P1.2; P1.6
and P1.7 after P1.5. Overseer step after wave 1: read the shadow log for at least
two days; every disagreement is explained or fixed before wave 2.

## Wave 2: cut the decision readers over (one bead each)

| # | Task | Files | Depends |
|---|---|---|---|
| P2.1 | `patrolScanHost.AgentRecord` reads the intent record, falling back to the bead only when the record has no finished-turn facts; one table test proves the seat decisions for every state are unchanged. This also re-points the kept-seat rule (gt-jw437c) | `internal/daemon/patrol_scan.go` | P1.6, shadow window |
| P2.2 | The pool dead-hooked count, seat cap and sandbox seat end read `work_bead` from the intent record | `internal/cmd/pool_seats.go`, `internal/daemon/patrol_scan_seatcap.go`, `internal/daemon/sandbox_seat_end.go` | P1.7, shadow window |
| P2.3 | The polecat manager's work-state, reclaim and hook decisions read the intent record | `internal/polecat/workstate.go`, `internal/polecat/hookbead.go`, `internal/polecat/reclaim.go` | P2.1 |
| P2.4 | The reaper's MR-wisp protection names live MR wisps from the work bead or the intent record, not `active_mr`; decided from P1.1's verdict | `internal/reaper/reaper.go` | P1.1 |

## Wave 3: stop the writers

| # | Task | Files | Depends |
|---|---|---|---|
| P3.1 | The supervisor stops mirroring seat state onto the agent bead | `internal/daemon/supervisor_host.go` | P2.1 |
| P3.2 | `gt done` and `gt sling` stop writing the agent bead | `internal/cmd/done_agent_state.go`, `internal/done/done.go`, `internal/cmd/sling_helpers.go` | P2.1, P2.2 |
| P3.3 | The polecat manager stops creating, reusing and updating agent beads; a reused name is `MarkSlung` on the intent record | `internal/polecat/manager.go`, `internal/polecat/agent_bead_memo.go` | P2.3, P3.2 |
| P3.4 | The overrun guard stops writing the agent bead's cleanup status | `internal/daemon/overrun_guard_stop.go` | P1.4 |
| P3.5 | `gt agent pause` stops writing the agent bead state | `internal/cmd/agent_pause.go` | P2.1 |

## Wave 4: the display readers and dead code

| # | Task | Files | Depends |
|---|---|---|---|
| P4.1 | `gt polecat list`, the dashboard, town status and capacity read the intent record | `internal/cmd/polecat.go`, `internal/cmd/dashboard_polecats.go`, `internal/townstatus/townstatus.go`, `internal/cmd/polecat_capacity.go` | P3.3 |
| P4.2 | The host-side sandbox stop hook reads the intent record, not the agent bead | `internal/cmd/tap_polecat_stop_sandbox.go` | P3.2 |
| P4.3 | Remove the agent-bead lookups in mail address resolution and the mail directory (mail is retired) | `internal/mail/resolve.go`, `internal/cmd/mail_directory.go` | none |
| P4.4 | Remove the doctor checks that audit agent beads | `internal/doctor/agent_beads_check.go`, `internal/doctor/agent_beads_shadow_check.go`, `internal/doctor/stale_agent_beads_check.go` | P3.3 |
| P4.5 | Remove the identity reconcile and identity commands' agent-bead reads, keeping `--delete-only` | `internal/cmd/polecat_identity.go`, `internal/cmd/polecat_identity_reconcile.go` | P4.1 |

## Wave 5: delete

| # | Task | Owner | Depends |
|---|---|---|---|
| P5.1 | Dry run of `gt polecat identity reconcile --delete-only` over the finished polecat records, pasted into the bead | overseer (attended) | P4.5 |
| P5.2 | Delete the finished polecat records; crew records in the same step | overseer (attended) | P5.1 |
| P5.3 | Remove the agent-bead API from `internal/beads`: types, create, reuse, listing, update helpers and their fakes and contract tests | polecat | P5.2 |
| P5.4 | Docs: `docs/concepts/identity.md`, the intent package doc and ADR 0003 say what is now true | polecat | P5.3 |

Overseer-owned, outside the repo: the backlog-sweep and check docs exclude the
`gt:agent` label from open-work counts until P5.2 lands.

## Order and parallelism

Wave 1 has two independent starts (P1.1, P1.2) and fans out to five beads. Wave 2
waits for the shadow window and is four small independent cuts after P2.1. Wave 3
can run most beads in parallel once wave 2 is in. Waves 4 and 5 are mostly
parallel, except that the deletes are attended and come last.

## Recovery

Until P3.x lands, the agent bead is still written, so a wrong cut is undone by
reverting that one reader. After wave 3 the agent bead is stale, so a revert of a
wave-2 reader is no longer safe: wave 3 does not start until every wave-2 reader
has run a full day on the intent record with no disagreement.
