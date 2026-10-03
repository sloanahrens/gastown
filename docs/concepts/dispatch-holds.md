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
| `needs-pro`, `needs-mayor-review`, `gt:needs-human` (or `needs-human`) | label |
| `MAYOR DESIGN DECISION`, `do not redispatch` | design or notes |
| `HOLD RELEASED` | a comment, lifting an earlier comment hold |

The markers are machine-read, so write one exactly as listed. A label matches
however it is typed (case-insensitive). A prose marker holds only where it is
asserted — at the start of a line, past any `#`, `>`, `-`, `*`, `+` or backtick
in front of it — with case, hyphens, underscores and spaces folded, so
`- do not redispatch` and `do not re-dispatch` both hold while a note that only
quotes the wording does not.

## The operator reservation

`operator` and a human assignee are the operator reservation: work the person
means to do by hand. Every agent is addressed by a slash-qualified address
(`gastown/polecats/onyx`, `gastown/crew/sloan`, `mayor/`), so any other assignee
is a person. `gt sling` refuses one without `--force` (gt-21pl0), and the paths
that would otherwise point the mayor at it skip it.

## Who reads a hold

**patrol_scan restart.** The daemon's `patrol_scan` tick asks
`dispatch.DispatchHoldFields` about the hooked bead of a polecat it is about to
restart (internal/daemon/patrol_scan.go), so a session it would otherwise raise
against held work stays down (gt-n38c6). The read covers status, labels,
assignee, design and notes. Comments are not read — `bd show --json` omits them
— so a comment-recorded hold does not park a restart.

**Spec dispatcher.** `gt spec dispatch`, the daemon's `spec_dispatch` tick,
filters its candidates in `specdispatch.Eligible`
(internal/specdispatch/dispatch.go): a bead is held when its status is not
`open` (so `deferred`, `pinned`, `hooked` and `in_progress`), when it carries
any assignee, or when it wears `gt:ready-to-land`, `needs-human`,
`needs-mayor-review`, `spec-dispatch-failed`, or a runtime-family label. Two
markers diverge from the table above, and both are open (gt-lxxo4): the
`operator` label on an unassigned bead is caught only later by `gt sling`'s
refusal, and `gt:needs-human` — the spelling the landing worker writes — is not
held at all. `needs-pro` is not a hold here either: it routes the bead to the
seat that reserves that label. Design and notes prose is not read.

**Idle-seat nudge.** `gt daemon dispatch-check` counts the ready beads the mayor
could sling and skips any that `dispatch.OperatorReservation` names
(internal/cmd/daemon_dispatch.go), so an operator-labeled or human-assigned bead
is never named in the nudge.

**gt sling.** Refuses, without `--force`: a bead reserved for the operator, a
`deferred` bead, and a `pinned`, `hooked` or `in_progress` bead whose holder's
session is still live.

## Releases

Release a field hold by editing the field that carries it: clear the label,
reassign to an agent, set the status back to `open`, or remove the prose. A
comment is the one field that cannot be edited, so the rule pairs it with a
release marker: a later comment starting `HOLD RELEASED` lifts an earlier one.
No automatic path reads comments today — `dispatch.HoldInComments` has had no
caller since the convoy feeders were deleted (gt-gzhin.6, gt-thilk) — so write a
hold in notes or design to give it a reader.
