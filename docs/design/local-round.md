# Local round: the polecat's om review before gt done

A polecat can ask the daemon to run the landing reviewer over its branch head
before it submits. The answer arrives on the work bead as findings the polecat
can fix or dispute. `gt done` then refuses a submit that leaves a major or
critical finding unanswered. This page describes the flow, the bound, the
switch, the channel, what the round never writes, the failure rule and how to
turn the feature off.

## Flow

1. The polecat runs `make presubmit`, then `gt review-preview` from its
   worktree. The command pushes the branch head to origin when origin does not
   have it. It appends a `LOCAL ROUND REQUEST head=<sha>` note to the work bead
   and waits for a LOCAL ROUND block that answers that head. The wait is 15
   minutes, polled every 15 seconds. A request for a head that is already open
   is not appended again.
2. The daemon's local round patrol runs once a minute per rig. It looks at the
   open and in_progress beads, serves the oldest unanswered request first, and
   serves one request at a time. It fetches the head into a private worktree,
   runs the landing's om review over the diff from the merge base of main, and
   appends the result to the bead's notes.
3. The answer is a block headed `LOCAL ROUND (n): head=<sha>`, followed by one
   finding line per finding. If the round could not run, the answer is a single
   line, `LOCAL ROUND (n): unavailable - <reason>`, with no findings.
4. The polecat reads the findings, which `gt review-preview` prints with their
   ids. For each major or critical finding it either fixes the code and runs
   `gt review-preview reply <id> --fixed -m "<what changed>"`, or disputes the
   finding with `gt review-preview reply <id> --disputed -m "<reason>"`. A reply
   is a note line that follows the block.
5. The polecat commits any fixes and runs `gt done`. The gate described under
   Failure rule decides whether the submit goes on.

## Bound

The bound is two per bead per day: a bead gets two local rounds per local day. The third request on the same day
is answered as unavailable, with a reason that names the bound, and the polecat
submits with `gt done`. The count is kept per bead, in one state file for each
bead under the rig's runtime directory. A later day restarts the count. A
state file that cannot be read or parsed is an error, and the count is never
reset to zero to get past it.

A request whose head cannot be fetched is not a round. The patrol fetches the
head before it takes a count, so the count does not move, and the answer is
unavailable. A round that reaches the reviewer and then fails, by an error or by
the wall bound, has used one of the day's rounds.

## Switch

The switch is `daemon.local_round.enabled` in the daemon section of the town's
configuration. It is off unless the town sets it to true. The wall bound for
one round is `daemon.local_round.timeout_seconds`. It defaults to 300 seconds,
and a value above 900 seconds is refused with an error that names the key.

When the switch is off, the patrol still answers each open request, with an
unavailable line that says the switch is off. A polecat's preview receives
that answer at the patrol's next tick and the submit goes ahead.

## Channel

The request, the block and the replies all travel as notes on the work bead.
The daemon already reads bead notes to find requests, `gt done` already reads
them to decide the gate, and the findings stay with the bead when a session
dies. A nudge is ephemeral, so a request sent by nudge could be lost before the
daemon served it. Mail writes a bead and a Dolt commit for every send, which is
too heavy for one request per head.

## What it never writes

A round writes no verdict, no review-log record and no rounds-ledger entry, and
it does not touch the rig's counters. The patrol fetches the head under a
private ref and never writes a landing ref, so a round cannot land anything.
The only write a round makes is the block or the unavailable line on the bead's
notes.

## Failure rule

An unavailable answer never blocks a submit. `gt review-preview` exits 0 and
prints that the preview is unavailable, and `gt done` passes when the last
LOCAL ROUND line on the bead is an unavailable line.

A major or critical finding that is unanswered always blocks a submit.
`gt done` refuses while the last round has such a finding and no reply for its
id follows the block. A reply counts only once the branch head has moved past
the round's head. Minor and info findings never block. No flag skips the gate.

## Turning it off

Set `daemon.local_round.enabled` to false in the daemon section of the town's
configuration. The daemon re-reads the patrol configuration on its next
heartbeat after the setting changes, and the patrol takes the switch in force
each time it serves a request, so a request it serves after that reload gets
the unavailable answer. Polecats then receive unavailable answers and submit
as they did before the feature existed.
