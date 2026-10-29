package dashboard

import "time"

// The strip's thresholds, the one place a reading turns into an alarm: the
// oldest landing still waiting to be landed reads amber after a quarter of an
// hour and red after one, and an install this many commits behind main is worth
// saying so. They are here rather than in the page so the alarm is a rule this
// package runs against the readings it holds.
const (
	stripWaitWarn   = 15 * time.Minute
	stripWaitBad    = time.Hour
	stripBehindWarn = 20
)

// The strip's two alarm tokens, which the page's own stylesheet defines for the
// strip: the reader's amber and red, the verdict the machine reading already
// carries in host_state.
const (
	alarmWarn = "warn"
	alarmBad  = "bad"
)

// polecatAlarm is the alarm the strip wears for a polecat's state. A quiet
// polecat holds work and has gone silent past QuietAfter, and a bead waiting
// on a person reads red under every person label (gt-slmaz); a polecat
// queued, spawning, parked or idle is the town working as intended. The reader
// learns a state the moment the town does, so a state with no row here earns
// no alarm rather than passing as a calm one, and the working fact names it in
// its title instead.
var polecatAlarm = map[string]string{
	StateWorking:       "",
	StateQuiet:         alarmWarn,
	StateGating:        "",
	StateQueued:        "",
	StateSpawning:      "",
	StateStalled:       alarmBad,
	StateReviewNeeded:  "",
	StateRecovery:      "",
	StateNeedsHuman:    alarmBad,
	StateNeedsOverseer: alarmBad,
	StateNeedsOperator: alarmBad,
	StateParked:        "",
	StateIdle:          "",
	StateUnknown:       "",
}

// StripAlarms is what each of the strip's alarming readings earned, decided
// here so the page draws a verdict rather than deriving one from a reading
// that has drifted since the reader took it. Empty means the reading is
// nothing to act on.
type StripAlarms struct {
	// Waiting is the queue of landings: the ready read's own state while it is
	// failed or aged, else the age of the oldest one still waiting.
	Waiting string `json:"waiting,omitempty"`
	// Working is the worst alarm any polecat holding a live reading earns.
	Working string `json:"working,omitempty"`
	// NeedsYou is the human's share of what is waiting on a decision: red while
	// a reading behind the count could not be made or a row is the human's,
	// amber while rows went unread that could be.
	NeedsYou string `json:"needs_you,omitempty"`
	// Behind is the installed gt's distance from main.
	Behind string `json:"behind,omitempty"`
}

// stripAlarms decides the verdict on each of the strip's alarming readings.
func stripAlarms(st *State) *StripAlarms {
	return &StripAlarms{
		Waiting:  stripWaitingAlarm(st),
		Working:  stripWorkingAlarm(st.Polecats),
		NeedsYou: stripNeedsYouAlarm(st.Attention),
		Behind:   stripBehindAlarm(st.Summary),
	}
}

// stripWaitingAlarm is the queue's alarm. A count whose read failed is red and
// one whose read has gone old is amber, whoever made it: a count the reader
// could not take is never a drained queue. Otherwise it is the age of the
// oldest landing still waiting, which is the reading the operator acts on.
func stripWaitingAlarm(st *State) string {
	sm := st.Summary
	if sm == nil {
		return ""
	}
	if r, ok := st.Reads[SourceReady]; ok && r.StaleNow(st.Now) {
		if r.Error {
			return alarmBad
		}
		return alarmWarn
	}
	if sm.OldestReady == nil {
		return ""
	}
	return ageAlarm(st.Now.Sub(*sm.OldestReady), stripWaitWarn, stripWaitBad)
}

// stripWorkingAlarm is the worst alarm any polecat earns. A parked rig is
// stood down on purpose and holds nothing, so its polecats earn nothing; no
// polecats read at all is an alarm nobody has earned.
func stripWorkingAlarm(polecats []Polecat) string {
	worst := ""
	for _, p := range polecats {
		if p.RigParked {
			continue
		}
		if a := polecatAlarm[p.State]; alarmRank(a) > alarmRank(worst) {
			worst = a
		}
	}
	return worst
}

// stripNeedsYouAlarm is the human's share of what is waiting: red while a
// reading behind the list could not be made, nothing while one of them has not
// come back, and red while the count is up. Rows the readers capped out were
// never read for an owner, so a count of none beside them is a count nobody
// made rather than a human with nothing to do: it reads amber (gt-bi6w0).
func stripNeedsYouAlarm(at *Attention) string {
	if at == nil {
		return ""
	}
	if len(at.Failed) > 0 {
		return alarmBad
	}
	if len(at.Pending) > 0 {
		return ""
	}
	if at.Mine > 0 {
		return alarmBad
	}
	if at.More > 0 {
		return alarmWarn
	}
	return ""
}

// stripBehindAlarm is the installed gt's distance from main. An install nobody
// has read is not a current one.
func stripBehindAlarm(sm *Summary) string {
	if sm == nil || sm.Behind == nil || *sm.Behind <= stripBehindWarn {
		return ""
	}
	return alarmWarn
}

// ageAlarm is the alarm an age earns: red past bad, amber past warn, nothing
// below both.
func ageAlarm(age, warn, bad time.Duration) string {
	switch {
	case age >= bad:
		return alarmBad
	case age >= warn:
		return alarmWarn
	}
	return ""
}

// alarmRank orders the strip's alarms so the worst of a set of readings is the
// one the strip wears.
func alarmRank(alarm string) int {
	switch alarm {
	case alarmBad:
		return 2
	case alarmWarn:
		return 1
	}
	return 0
}
