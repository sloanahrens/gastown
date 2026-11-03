package specdispatch

import (
	"fmt"
	"regexp"
	"strings"
)

// adviceTriggerRe matches the words that name a step whose failure behavior
// the lint wants stated: a probe, a guard, a hold, a drain, an install, a
// lock, a lease, a gc step or a heartbeat. Each word matches at its start
// (\b) so its plural and inflections count ("probes", "holding", "installs")
// while an occurrence inside another word does not ("block" is not a lock,
// "threshold" is not a hold).
var adviceTriggerRe = regexp.MustCompile(`(?i)\b(probe|guard|hold|drain|install|lock|lease|gc|heartbeat)`)

// adviceFailureRe matches the words an acceptance box uses to name what
// happens when a read or step fails. It is read the same way as the trigger:
// word-start only, so "fails", "errors" and "timeouts" count.
var adviceFailureRe = regexp.MustCompile(`(?i)\b(error|fail|refuse|unreadable|cannot|timeout)`)

// adviceFailureText is the sentence both the package and its caller show for
// the failure-behavior advisory.
const adviceFailureText = "the Goal or Constraints mentions %q but no acceptance box says what happens when the read or step fails: add a box naming the failure behavior"

// Advice returns the lint's advisories for s: one-line notes that name a gap
// worth closing without refusing the bead. It is pure — it reads s and
// nothing else — so a caller can print advice beside any verdict, and advice
// never changes a route or an exit code.
//
// The one advisory so far is the failure behavior of the most expensive
// steps. A bead whose Goal or Constraints names a probe, guard, hold, drain,
// install, lock, lease, gc step or heartbeat is one whose worst defects were
// unstated fail-open behavior (a probe read error treated as "nothing to
// wait for"). When no acceptance box names an error, a failure, a refusal, an
// unreadable input, a "cannot" or a timeout, Advice says to add one.
func Advice(s Spec) []string {
	sections := Sections(s.Description)
	trigger := strings.Join([]string{sections["goal"], sections["constraints"]}, "\n")
	match := adviceTriggerRe.FindString(trigger)
	if match == "" {
		return nil
	}
	for _, item := range acceptanceItemTexts(acceptanceText(s, sections)) {
		if adviceFailureRe.MatchString(item) {
			return nil
		}
	}
	return []string{fmt.Sprintf(adviceFailureText, strings.ToLower(match))}
}
