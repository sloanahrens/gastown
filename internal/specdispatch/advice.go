package specdispatch

import (
	"fmt"
	"regexp"
	"strings"
)

// adviceTriggerRe matches the words that name a step whose failure behavior
// the lint wants stated: a probe, a guard, a hold, a drain, an install, a
// lock, a lease, a gc step or a heartbeat. Each word matches whole, in the
// forms a bead writes it ("probes", "probing", "holding", "installs",
// "installation", "locked"), so a longer token that starts with a trigger
// does not count ("lockfile" is not a lock, "installer" is not an install,
// "gcloud" is not a gc step), and a word that contains one does not either
// ("block" is not a lock, "threshold" is not a hold).
var adviceTriggerRe = regexp.MustCompile(`(?i)\b(probes?|probed|probing|guards?|guarded|guarding|holds?|holding|held|drains?|drained|draining|installs?|installed|installing|installations?|locks?|locked|locking|leases?|leased|leasing|gc|gcs|heartbeats?)\b`)

// adviceFailureRe matches the words an acceptance box uses to name what
// happens when a read or step fails. It is read the same way as the trigger:
// word-start only, so "fails", "errors" and "timeouts" count. A failure word
// names a read or step failure only in a box that also matches
// adviceReadStepRe.
var adviceFailureRe = regexp.MustCompile(`(?i)\b(error|fail|refuse|unreadable|cannot|timeout)`)

// adviceReadStepRe matches the words that name the read or step a failure
// would happen in. An acceptance box satisfies the advisory only when it pairs
// one of adviceFailureRe's words with one of these, so "the column shows the
// error count" on a probe bead does not.
var adviceReadStepRe = regexp.MustCompile(`(?i)\b(read|reads|reading|unreadable|step|steps|probe|probes|probed|probing|query|queries|fetch|fetches|input|inputs)\b`)

// adviceFailureText is the sentence Advice returns for the failure-behavior
// advisory; the caller prints it as one "advice:" line.
const adviceFailureText = "the Goal or Constraints mentions %q but no acceptance box says what happens when the read or step fails: add a box naming the failure behavior"

// adviceExcludedLabelText is the sentence Advice returns for a bead an excluded
// label keeps off the dispatcher. Callers outside this package get it only
// through Advice, so they never read this constant.
const adviceExcludedLabelText = "the dispatcher will not take this bead: it wears label %s"

// Advice returns the lint's advisories for s: one-line notes that name a gap
// worth closing without refusing the bead. It is pure — it reads s and the
// package's excluded-label list, nothing else — so a caller can print advice
// beside any verdict, and advice never changes a route or an exit code.
//
// The failure-behavior advisory: a bead whose Goal or Constraints names a
// probe, guard, hold, drain, install, lock, lease, gc step or heartbeat is one
// whose worst defects were unstated fail-open behavior (a probe read error
// treated as "nothing to wait for"). When no acceptance box pairs an error, a
// failure, a refusal, an unreadable input, a "cannot" or a timeout with a read
// or a step, Advice says to add one.
//
// The excluded-label advisory: a bead wearing a label in ExcludedLabels() is
// one the dispatcher never takes, so a clean lint result would overstate its
// readiness. Advice names each such label on its own line, matching the
// label as Spec.HasLabel does; the labels are read from ExcludedLabels(), so
// the advice and the dispatcher cannot disagree on the set.
func Advice(s Spec) []string {
	sections := Sections(s.Description)
	trigger := strings.Join([]string{sections["goal"], sections["constraints"]}, "\n")
	var advice []string
	if match := adviceTriggerRe.FindString(trigger); match != "" && !adviceNamesFailure(s, sections) {
		advice = append(advice, fmt.Sprintf(adviceFailureText, strings.ToLower(match)))
	}
	for _, label := range ExcludedLabels() {
		if s.HasLabel(label) {
			advice = append(advice, fmt.Sprintf(adviceExcludedLabelText, label))
		}
	}
	return advice
}

// adviceNamesFailure reports whether one acceptance box names what happens
// when a read or step fails: a failure word and a read or step word in the
// same box.
func adviceNamesFailure(s Spec, sections map[string]string) bool {
	for _, item := range acceptanceItemTexts(acceptanceText(s, sections)) {
		if adviceFailureRe.MatchString(item) && adviceReadStepRe.MatchString(item) {
			return true
		}
	}
	return false
}
