package daemonimage

import "fmt"

// BDSamplesFloor is the oldest bd commit the daemon image may be pinned to: the
// beads commit that has `bd sample` and `bd mol bond --root-only`. The rule is
// that the pin descends from the samples build, not that the pin is the samples
// build (gt-tgje6t): a later bd commit keeps both verbs, while a pin back at the
// schema-66 build is what gt-fenu9p reverted.
const BDSamplesFloor = "0f9f640fe0f1a4162f3aebdef7994135b1167a85"

// CheckBDFloor judges one compare answer against BDSamplesFloor. onlyInFloor is
// the commit count of a compare whose base is tip and whose head is
// BDSamplesFloor, which is 0 exactly when the floor is an ancestor of the tip;
// that direction matters, since an inverted compare counts the tip's own
// commits. It returns nil for a count of 0. A compare error, a negative count or
// a count above 0 is a "bd-floor guard:" error naming the floor and the tip. The
// negative and positive counts are judgments, returned as refusals (Refusef, so
// errors.Is(err, ErrPinRefused)); a compare error is wrapped and is not one.
func CheckBDFloor(tip string, onlyInFloor int, err error) error {
	if err != nil {
		return fmt.Errorf("bd-floor guard: cannot compare bd tip %s with the samples build %s: %w", tip, BDSamplesFloor, err)
	}
	if onlyInFloor < 0 {
		return Refusef("bd-floor guard: bd tip %s against the samples build %s: commit count %d is negative", tip, BDSamplesFloor, onlyInFloor)
	}
	if onlyInFloor > 0 {
		return Refusef("bd-floor guard: bd tip %s does not contain the samples build %s: %d commit(s) reachable only from the floor", tip, BDSamplesFloor, onlyInFloor)
	}
	return nil
}
