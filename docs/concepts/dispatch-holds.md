# Dispatch holds

A **dispatch hold** is a decision recorded on a bead's own record that takes it
off the automatic dispatch path, so a dispatcher's default sling does not
override it. `internal/dispatch` defines the vocabulary
(`DispatchHoldFields`, `OperatorReservation`); this page is the write form, and
which automatic path reads which marker.

## The markers

| Marker, as written | Field it is written in |
|--------------------|------------------------|
| `deferred`, `pinned` | status |
| `operator` | label |
| a person's handle (`sloan`, `Sloan Ahrens`, `overseer`) | assignee |
| `needs-pro`, `gt:needs-human` (or `needs-human`) | label |
| `do not redispatch` | design or notes |

The markers are machine-read, so write one exactly as listed. A label matches
however it is typed (case-insensitive). A prose marker holds only where it is
asserted — at the start of a line, past any `#`, `>`, `-`, `*`, `+` or backtick
in front of it — with case, hyphens, underscores and spaces folded, so
`- do not redispatch` and `do not re-dispatch` both hold while a note that only
quotes the wording does not.

## The operator reservation

`operator` and a human assignee are the operator reservation: work the person
means to do by hand. Every agent is addressed by a slash-qualified address
(`gastown/polecats/onyx`, `gastown/crew/sloan`, `gastown/witness`), so any other
assignee is a person. `gt sling` refuses one without `--force` (gt-21pl0), and
every automatic dispatcher skips it.

## Who reads a hold

**patrol_scan restart.** The daemon's `patrol_scan` tick asks
`dispatch.DispatchHoldFields` about the hooked bead of a polecat it is about to
restart (internal/daemon/patrol_scan.go), so a session it would otherwise raise
against held work stays down (gt-n38c6). The read covers status, labels,
assignee, design and notes; comments are not part of the rule (`bd show --json`
omits them).

**Spec dispatcher.** `gt spec dispatch`, the daemon's `spec_dispatch` tick,
filters its candidates in `specdispatch.Eligible`
(internal/specdispatch/dispatch.go): a bead is held when its status is not
`open` (so `deferred`, `pinned`, `hooked` and `in_progress`), when it carries
any assignee, when it wears `gt:ready-to-land`, `spec-dispatch-failed` or a
runtime-family label, or when `dispatch.DispatchHoldFields` asserts a hold on
its fields — the table above, read rather than copied (gt-lxxo4).

One marker in that table is not a hold here: `needs-pro` is the pro seat's
selector (`polecat_pool.pro_label`), so it routes the bead to the seat that
reserves it. `Eligible` reads the rule with the seat labels dropped, which
means a `needs-pro` bead is held once no seat reserves the label.

The dispatcher also holds a bead whose notes carry a live `READY TO LAND`
block — one no `LANDING RECORD` and no `MERGE REJECTION` follows (gt-kr5xv).
`gt done` writes that block before the `gt:ready-to-land` label, and a label is
the half of a ready-board answer that has gone missing before (gt-q6zoo), so
between the two writes the bead is one the landing worker is already taking
while its label is absent. The block is the half a read that misses the label
still carries.

**Dispatchable-work fold.** The ready board's dispatchable-work predicate skips
any bead `dispatch.OperatorReservation` names (internal/cmd/dispatchable_work.go),
so an operator-labeled or human-assigned bead is never counted as work a
dispatcher could pick up.

**gt sling.** Refuses, without `--force`: a bead reserved for the operator, a
`deferred` bead, and a `pinned`, `hooked` or `in_progress` bead whose holder's
session is still live.

## Releases

Release a hold by editing the field that carries it: clear the label, reassign
to an agent, set the status back to `open`, or remove the prose. A comment
carries no hold — no automatic path reads one — so write a hold in notes or
design to give it a reader.
