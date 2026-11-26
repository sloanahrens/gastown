package rigaudit

import (
	"fmt"
	"strings"
)

// Verdict renders the audit's last line and reports whether the town passes.
// ok is true when no problem is harder than info. The line counts the unparked
// rigs and the info findings, and names the parked rigs.
func Verdict(problems []Problem, unparked, parked []string) (line string, ok bool) {
	hard, info := 0, 0
	for _, p := range problems {
		if p.Kind == "info" {
			info++
		} else {
			hard++
		}
	}
	names := "none"
	if len(parked) > 0 {
		names = strings.Join(parked, ", ")
	}
	if hard == 0 {
		return fmt.Sprintf("RIG AUDIT: OK (%d unparked, %d parked: %s; %d info)", len(unparked), len(parked), names, info), true
	}
	return fmt.Sprintf("RIG AUDIT: %d problem(s) (%d unparked, %d parked: %s; %d info)", hard, len(unparked), len(parked), names, info), false
}
