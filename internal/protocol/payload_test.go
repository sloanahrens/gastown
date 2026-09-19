package protocol

import "testing"

// refineryRecoveredBeadBody is the dead-worker RECOVERED_BEAD template the
// refinery sends to the deacon (internal/refinery/dead_worker_recovery.go).
// It is the payload from gt-8sex: the body a deacon received in place of the
// prose a refinery had typed into `gt mail reply -m`.
const refineryRecoveredBeadBody = `Merge rejection with no live worker (transient polecat).

Bead: gt-ntqf
Polecat: gastown/polecats/flint
MR: gt-wisp-c12
Branch: polecat/flint/gt-ntqf
Failure-Type: tests
Attempt: 1

The source bead has been reopened with merge-rejection notes.
Please re-dispatch. The branch survives on origin, so the next polecat
can check it out and make a targeted fix instead of starting over.`

// witnessRecoveredBeadBody is the witness's RECOVERED_BEAD template
// (internal/witness/handlers.go), which carries a different field set.
const witnessRecoveredBeadBody = `Recovered abandoned bead from dead polecat.

Bead: gt-abc
Polecat: gastown/dusty
Previous Status: running
Respawn Count: 1

The bead has been reset to open with no assignee.
Please re-dispatch to an available polecat.`

func TestLooksLikeProtocolPayload(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"refinery recovered bead", refineryRecoveredBeadBody, true},
		{"witness recovered bead", witnessRecoveredBeadBody, true},
		{"merge ready", formatMergeReadyBody(MergeReadyPayload{
			Branch:   "polecat/nux/gt-abc",
			Issue:    "gt-abc",
			Polecat:  "nux",
			Rig:      "gastown",
			Verified: "clean git state, issue closed",
		}), true},
		{"fix needed", formatFixNeededBody(FixNeededPayload{
			Branch:        "polecat/nux/gt-abc",
			Issue:         "gt-abc",
			Polecat:       "nux",
			Rig:           "gastown",
			TargetBranch:  "main",
			FailureType:   "tests",
			Error:         "TestFoo failed",
			MRBeadID:      "gt-wisp-abc",
			AttemptNumber: 1,
		}), true},

		{"prose acknowledgement", "Ack on the closure, and one substantive addendum.\n" +
			"I re-ran the checks read-only while waiting on the MRs; three of four fail.", false},
		{"prose that names one field", "Branch: main looks right to me, shipping it.", false},
		{"prose quoting a payload", "Here is what the deacon sent:\n\n" +
			"> Bead: gt-ntqf\n> Failure-Type: tests\n\nI have re-dispatched it.", false},
		{"prose with an indented payload", "The notification read:\n\n" +
			"    Bead: gt-ntqf\n    Failure-Type: tests\n\nNothing to do.", false},
		{"prose with a bulleted field", "Notes:\n- Branch: main\n- Issue: gt-abc\nNothing else.", false},
		{"empty", "", false},
		{"subject line only", "RECOVERED_BEAD gt-ntqf", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeProtocolPayload(tc.body); got != tc.want {
				t.Errorf("LooksLikeProtocolPayload() = %v, want %v for body:\n%s", got, tc.want, tc.body)
			}
		})
	}
}
