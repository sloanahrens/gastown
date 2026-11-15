> Status: historical (2026-11). Abandoned: the local round is retired by operator ruling 2026-11-14 (gt-dpl81b) — it repeated the landing review, and one seat waited eight minutes on it. Not maintained.

# Local round: the polecat's om review before gt done

The local round was a polecat's request that the daemon run the landing reviewer
over its branch head before it submitted, so the answer's findings could be
fixed or disputed before `gt done`. The command `gt review-preview`, the review
code and the daemon patrol were deleted in gt-755l63. This page records the
design as it stood: the flow, the bound, the switch, the channel, what the round
never wrote, the failure rule and how the feature was turned off.

## Flow

1. The polecat ran `make presubmit`, then `gt review-preview` from its
   worktree. The command pushed the branch head to origin when origin did not
   have it. It appended a `LOCAL ROUND REQUEST head=<sha>` note to the work bead
   and waited for a LOCAL ROUND block that answered that head. The wait was 15
   minutes, polled every 15 seconds. A request for a head that was already open
   was not appended again.
2. The daemon's local round patrol ran once a minute per rig. It looked at the
   open and in_progress beads, served the oldest unanswered request first, and
   served one request at a time. It fetched the head into a private worktree,
   ran the landing's om review over the diff from the merge base of main, and
   appended the result to the bead's notes.
3. The answer was a block headed `LOCAL ROUND (n): head=<sha>`, followed by one
   finding line per finding. If the round could not run, the answer was a single
   line, `LOCAL ROUND (n): unavailable - <reason>`, with no findings.
4. The polecat read the findings, which `gt review-preview` printed with their
   ids. For each major or critical finding it either fixed the code and ran
   `gt review-preview reply <id> --fixed -m "<what changed>"`, or disputed the
   finding with `gt review-preview reply <id> --disputed -m "<reason>"`. A reply
   was a note line that followed the block.
5. The polecat committed any fixes and ran `gt done`. The gate described under
   Failure rule decided whether the submit went on.

## Bound

The bound was two per bead per day: a bead got two local rounds per local day. The third request on the same day
was answered as unavailable, with a reason that named the bound, and the polecat
submitted with `gt done`. The count was kept per bead, in one state file for each
bead under the rig's runtime directory. A later day restarted the count. A
state file that could not be read or parsed was an error, and the count was never
reset to zero to get past it.

A request whose head could not be fetched was not a round. The patrol fetched the
head before it took a count, so the count did not move, and the answer was
unavailable. A round that reached the reviewer and then failed, by an error or by
the wall bound, used one of the day's rounds.

## Switch

The switch was `daemon.local_round.enabled` in the daemon section of the town's
configuration. It was off unless the town set it to true. The wall bound for
one round was `daemon.local_round.timeout_seconds`. It defaulted to 300 seconds,
and a value above 900 seconds was refused with an error that named the key.

When the switch was off, the patrol still answered each open request, with an
unavailable line that said the switch was off. A polecat's preview received
that answer at the patrol's next tick and the submit went ahead.

## Channel

The request, the block and the replies all travelled as notes on the work bead.
The daemon read bead notes to find requests, `gt done` read them to decide the
gate, and the findings stayed with the bead when a session died. A nudge was
ephemeral, so a request sent by nudge could be lost before the daemon served it.
Mail wrote a bead and a Dolt commit for every send, which was too heavy for one
request per head.

## What it never wrote

A round wrote no verdict, no review-log record and no rounds-ledger entry, and
it did not touch the rig's counters. The patrol fetched the head under a
private ref and never wrote a landing ref, so a round could not land anything.
The only write a round made was the block or the unavailable line on the bead's
notes.

## Failure rule

An unavailable answer never blocked a submit. `gt review-preview` exited 0 and
printed that the preview was unavailable, and `gt done` passed when the last
LOCAL ROUND line on the bead was an unavailable line.

A major or critical finding that was unanswered always blocked a submit.
`gt done` refused while the last round had such a finding and no reply for its
id followed the block. A reply counted only once the branch head had moved past
the round's head. Minor and info findings never blocked. No flag skipped the gate.

## Turning it off

The feature was turned off by setting `daemon.local_round.enabled` to false in
the daemon section of the town's configuration. The daemon re-read the patrol
configuration on its next heartbeat after the setting changed, and the patrol
took the switch in force each time it served a request, so a request it served
after that reload got the unavailable answer. Polecats then received unavailable
answers and submitted as they did before the feature existed.
