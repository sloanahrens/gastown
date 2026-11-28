package main

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// doctorAnswers is nil when the target's doctor holds a rung, and an error naming
// the reason when it is down or the doctor fails.
func doctorAnswers(t target) error {
	diag, err := t.Doctor()
	if err != nil {
		return err
	}
	if diag.Rung == gtwrap.RungDown {
		return fmt.Errorf("the target does not answer: %s", diag.Reason)
	}
	return nil
}
