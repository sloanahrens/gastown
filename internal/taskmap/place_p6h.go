package taskmap

// Placements for batch P6 piece H (gt-ic36di): the update job that installs the
// newest build and runs the town's maintenance, the command that records the
// daemon and polecat image digests for one green commit, the rollback that
// returns the daemon to the pair its last digest install replaced, and the
// reader of the last run's record. Group judgement: update sits in host beside
// gt update bd-base, because it changes the binary the machine runs; images and
// rollback sit in roll, which covers moving a landed commit on to its next
// target and undoing that move; update status sits in host beside gt daemon
// status, because it reports the state of that run on this machine.
func init() {
	Register(
		Entry{Path: "update", Group: "host", Use: "Use this when the town should move to the newest build and its maintenance, from draining the working polecats through the daemon's cycle to installing and verifying the new binary.", Check: "the run prints its plan before it starts and the steps and outcome at the end; exit 0 is an ok run or a dry run, 1 failed, 3 partial, 4 aborted and 5 when another run holds the lock"},
		Entry{Path: "update images", Group: "roll", Use: "Use this when one green commit's daemon and polecat images are built and their digests must be recorded, so gt update --images can move the daemon to that commit's pair.", Check: "the run prints wrote <path> for <commit>, or says <path> already records <commit>; nothing changed when the pair is unchanged"},
		Entry{Path: "update rollback", Group: "roll", Use: "Use this when the daemon came up on a bad image pair and must go back to the pair its last digest install replaced.", Check: "exit 0 means the daemon runs the restored pair, 1 a refusal or a failed restore, and 5 that another update run holds the lock"},
		Entry{Path: "update status", Group: "host", Use: "Use this when you need to know what the last or current gt update was doing, how old its heartbeat is and which step it reached, before you retry, interrupt or roll it back.", Check: "the report prints run <id> (<source>): <outcome> with one line per step, and a town with no record prints no gt update has run"},
	)
}
