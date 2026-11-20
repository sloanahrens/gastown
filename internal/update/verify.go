package update

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
)

const (
	checkGTVersion         = "gt-version"
	checkDaemonVersion     = "daemon-version"
	checkImageLabel        = "image-label"
	checkHealth            = "health"
	checkPolecatAnnotation = "polecat-annotation"
	checkDevTag            = "dev-tag"
)

// shortCommitLen is the shortest commit string treated as authoritative: a
// prefix shorter than this is too little to identify a build.
const shortCommitLen = 7

// VerifyProbes is how a run asks the town what it is running.
type VerifyProbes interface {
	// GTVersion is the installed gt's commit.
	GTVersion(ctx context.Context) (string, error)
	// DaemonVersion is the running daemon's commit.
	DaemonVersion(ctx context.Context) (string, error)
	// ImageLabel is the org.gastown.gt-commit label of the sandbox image.
	ImageLabel(ctx context.Context) (string, error)
	// Health is the town health line, for example "GREEN tick 2m ago".
	Health(ctx context.Context) (string, error)
}

// PairProbes is the optional extension of VerifyProbes a run of an image pair
// implements: the polecat digest the run wants and the two records of which
// polecat image the town is actually running.
type PairProbes interface {
	// WantPolecat is the polecat digest the pair is rolling to.
	WantPolecat() string
	// PolecatAnnotation is the Deployment's polecat-image annotation.
	PolecatAnnotation(ctx context.Context) (string, error)
	// DevTagDigest is the digest the sandbox registry's dev tag points at.
	DevTagDigest(ctx context.Context) (string, error)
}

// CrashProbes is the optional extension of VerifyProbes a run implements when it
// can tell whether the daemon's newest pod is crash-looping.
type CrashProbes interface {
	// PodCrashing reports whether the daemon's newest pod is crash-looping and,
	// when it is, why: the container and the reason, for example
	// "gt: CrashLoopBackOff".
	PodCrashing(ctx context.Context) (crashing bool, why string, err error)
}

// ReadyProbes is the optional extension of VerifyProbes a run implements when it
// can ask the town the question readiness answers: whether the town can take
// traffic, given a report whose non-green fields it reads in full rather than
// through the health line's lossy rendering. A run that implements it decides
// the health check by this answer, as long as the line reads as a health line;
// one that does not keeps the line's rule.
type ReadyProbes interface {
	// Ready reports whether the town is ready and, when it is not, why: the
	// reason a person reads in the check's detail.
	Ready(ctx context.Context) (ready bool, reason string, err error)
}

// CrashLoopError is why a restart wait ended before its bound: the daemon's
// newest pod is crash-looping, so it cannot report the new build however long
// the wait lasts. The caller tells it apart from a canceled or failed sleep
// with errors.As, because the wait is over either way but only this one means
// the new build cannot come up.
type CrashLoopError struct {
	// Why is the container and the reason the crash probe reported, for
	// example "gt: CrashLoopBackOff". It may be empty.
	Why string
}

func (e *CrashLoopError) Error() string {
	if why := strings.TrimSpace(e.Why); why != "" {
		return "the daemon's new pod is crash-looping: " + why
	}
	return "the daemon's new pod is crash-looping"
}

// Check is one result of Verify: the fact checked, whether it matched, and
// what the probe reported.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Verify asks every probe what the town is running and reports four checks in
// a fixed order; a probe error fails its own check and leaves the others
// evaluated. Probes that also implement PairProbes add the two checks that make
// a roll usable for dispatch, polecat-annotation and dev-tag, after the four.
func Verify(ctx context.Context, p VerifyProbes, want string) []Check {
	checks := []Check{
		versionCheck(ctx, checkGTVersion, p.GTVersion, want),
		versionCheck(ctx, checkDaemonVersion, p.DaemonVersion, want),
		versionCheck(ctx, checkImageLabel, p.ImageLabel, want),
		healthCheck(ctx, p),
	}
	pair, ok := p.(PairProbes)
	if !ok {
		return checks
	}
	wantPolecat := pair.WantPolecat()
	return append(checks,
		digestCheck(ctx, checkPolecatAnnotation, pair.PolecatAnnotation, wantPolecat),
		digestCheck(ctx, checkDevTag, pair.DevTagDigest, wantPolecat),
	)
}

// VerifyWithSettle runs Verify once, then re-reads a health line that classifies
// as settling until it passes (naming the settle time) or wait has passed since
// the first read, failing with the last line and "still settling after <wait>";
// any other answer ends it at once; a failed sleep returns its error.
func VerifyWithSettle(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p VerifyProbes, want string, wait, poll time.Duration) ([]Check, error) {
	checks := Verify(ctx, p, want)
	health := &checks[3] // Verify reports the four checks in a fixed order; health is fourth.
	if health.OK || classifyHealthLine(health.Detail) == healthFail {
		return checks, nil
	}
	start := clock.Now()
	for {
		if clock.Now().Sub(start) >= wait {
			health.Detail += fmt.Sprintf(" (still settling after %s)", wait)
			return checks, nil
		}
		if err := sleep(ctx, poll); err != nil {
			return checks, err
		}
		line, err := p.Health(ctx)
		if err != nil {
			*health = Check{Name: checkHealth, Detail: err.Error()}
			return checks, nil
		}
		*health = Check{Name: checkHealth, Detail: line}
		switch classifyHealthLine(line) {
		case healthPass:
			health.OK = true
			health.Detail += fmt.Sprintf(" (settled after %s)", clock.Now().Sub(start))
			return checks, nil
		case healthFail:
			return checks, nil
		}
	}
}

// WaitForRestart polls the daemon until it reports want or timeout passes,
// which is measured from the first poll. The poll that finds the timeout passed
// is the last one, so a sleep that ends on the timeout still gets its poll; a
// canceled context or a failing sleep returns that error. A poll first asks the
// daemon for its version: a version that matches want ends the wait as a
// restart, before anything else, so a crash reading from a pod that has not
// been re-read cannot fail a restart that has already happened. A probe set
// that also implements CrashProbes is asked after that, on every poll that did
// not see want: a pod reported crash-looping ends the wait at once with healthy
// false and a *CrashLoopError carrying why, because a pod that cannot come up
// cannot report want inside the bound. A crash probe error leaves the wait
// running, since the pod is briefly unreadable while it starts or restarts, and
// is logged once for a pod that stays unreadable rather than on every poll.
func WaitForRestart(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p VerifyProbes, want string, timeout, poll time.Duration) (bool, error) {
	start := clock.Now()
	crash, watchesCrash := p.(CrashProbes)
	probeErrored := false
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// A probe error is expected while the daemon is down mid-restart, so
		// only a version that matches ends the wait.
		if version, err := p.DaemonVersion(ctx); err == nil && sameCommit(version, want) {
			return true, nil
		}
		if watchesCrash {
			crashing, why, err := crash.PodCrashing(ctx)
			switch {
			case err != nil:
				if !probeErrored {
					log.Printf("update: waiting for the daemon restart: the crash probe failed: %v", err)
					probeErrored = true
				}
			case crashing:
				return false, &CrashLoopError{Why: why}
			default:
				probeErrored = false
			}
		}
		if clock.Now().Sub(start) >= timeout {
			return false, nil
		}
		if err := sleep(ctx, poll); err != nil {
			return false, err
		}
	}
}

// versionCheck compares one probe's commit with want.
func versionCheck(ctx context.Context, name string, probe func(context.Context) (string, error), want string) Check {
	got, err := probe(ctx)
	if err != nil {
		return Check{Name: name, Detail: err.Error()}
	}
	if !sameCommit(got, want) {
		return Check{Name: name, Detail: fmt.Sprintf("got %q, want %q", got, want)}
	}
	return Check{Name: name, OK: true, Detail: got}
}

// digestCheck compares one probe's digest with want. A digest names one image
// exactly, so it has no short form: the trimmed strings must be equal. A wanted
// digest that is empty names no image, so every digest fails against it.
func digestCheck(ctx context.Context, name string, probe func(context.Context) (string, error), want string) Check {
	want = strings.TrimSpace(want)
	if want == "" {
		return Check{Name: name, Detail: "the pair named no polecat digest"}
	}
	got, err := probe(ctx)
	if err != nil {
		return Check{Name: name, Detail: err.Error()}
	}
	if got = strings.TrimSpace(got); got != want {
		return Check{Name: name, Detail: fmt.Sprintf("got %q, want %q", got, want)}
	}
	return Check{Name: name, OK: true, Detail: got}
}

// queueReasons are the health line's reasons that name a queue a person has to
// work through, not a fault: beads waiting for the overseer, the operator or a
// human, and an open escalation. Each maps to the check its value must pass,
// because the key alone does not make a reason a queue state: a person wait
// carries a count (needs-overseer=2) and the escalation field carries how old
// the oldest open escalation is (escalation=oldest_2h, its value as
// internal/townhealth renders it). The key list and the values are the ones the
// town writes — the three person labels of internal/townhealth's personFields
// and its escalation field — and a reason outside them fails. The escalation
// key is the singular townhealth.FieldEscalation, which is what the town
// renders (escalation=oldest_2h); a plural spelling is not a key the town
// writes and is refused (gt-dp3bbv).
var queueReasons = map[string]func(string) bool{
	"needs-overseer": isCount,
	"needs-operator": isCount,
	"needs-human":    isCount,
	"escalation":     isOldestAge,
}

// healthCheck verifies the install, not the town's queue. A probe error fails
// the check with the probe's message.
//
// For a probe set that does not implement ReadyProbes it passes on a GREEN line
// and on a DEGRADED line whose only reasons are queue states, naming the reasons
// it ignored. Any other verdict, a DEGRADED line with any other reason or with
// no reason, and a line that does not read "<VERDICT> tick <age> ago" fails, and
// the detail carries the line as read.
//
// A probe set that also implements ReadyProbes is asked the town's readiness
// question after a successful health read instead, and the readiness answer
// decides the check whatever the line's verdict; a line that does not read as a
// health line still fails. The roll is thus judged by readiness, so a red rig
// main on the line is reported beside the outcome rather than folded into it.
// See readyHealthCheck.
func healthCheck(ctx context.Context, p VerifyProbes) Check {
	line, err := p.Health(ctx)
	if err != nil {
		return Check{Name: checkHealth, Detail: err.Error()}
	}
	if ready, ok := p.(ReadyProbes); ok {
		return readyHealthCheck(ctx, ready, line)
	}
	queue, ok := queueOnlyHealthLine(line)
	if !ok {
		return Check{Name: checkHealth, Detail: line}
	}
	if len(queue) == 0 {
		return Check{Name: checkHealth, OK: true, Detail: line}
	}
	return Check{Name: checkHealth, OK: true,
		Detail: fmt.Sprintf("%s (queue states, not a failed roll; ignored %s)", line, strings.Join(queue, ", "))}
}

// readyNote is what a passing ready check appends to a health line that is not
// GREEN, saying the line is reported beside the roll and is not part of it.
const readyNote = " (ready: the town's own recorded and queue states are reported beside the roll, not part of it)"

// readyHealthCheck passes the health check exactly when the town's readiness
// answer says it is ready, whatever the line's verdict, and its detail is the
// line as read; a line that is not GREEN carries the ready note. A line that
// does not read as a health line fails with the line as read before readiness
// is asked, so an unreadable line never passes. A not-ready answer fails naming
// the reason and the line, and a Ready error fails with the error text.
func readyHealthCheck(ctx context.Context, ready ReadyProbes, line string) Check {
	verdict, _, ok := healthLineHead(line)
	if !ok {
		return Check{Name: checkHealth, Detail: line}
	}
	isReady, reason, err := ready.Ready(ctx)
	if err != nil {
		return Check{Name: checkHealth, Detail: err.Error()}
	}
	if !isReady {
		return Check{Name: checkHealth, Detail: fmt.Sprintf("not ready: %s: %s", reason, line)}
	}
	if verdict == "GREEN" {
		return Check{Name: checkHealth, OK: true, Detail: line}
	}
	return Check{Name: checkHealth, OK: true, Detail: line + readyNote}
}

// queueOnlyHealthLine reports whether line is a health line a roll may pass on,
// and the queue reasons it ignored. A GREEN verdict passes whatever it lists;
// a DEGRADED one passes only when every reason it lists is a queue state, one
// of the queueReasons keys carrying the value its field writes. The reasons
// are the tokens after the colon, separated by commas or by spaces, so both
// the comma-separated form and the space-separated line the town renders read.
// Anything else — a RED or UNKNOWN verdict, a key outside queueReasons, a
// value that is not the one its field writes, a DEGRADED line with no reason, a
// line without the "<VERDICT> tick <age> ago" head — is not ok, so a shape
// this does not know fails rather than passes.
func queueOnlyHealthLine(line string) (queue []string, ok bool) {
	_, tokens, ok := healthReasons(line)
	if !ok {
		return nil, false
	}
	for _, reason := range tokens {
		if !isQueueState(reason) {
			return nil, false
		}
		queue = append(queue, reason)
	}
	return queue, true
}

// isQueueState reports whether reason is a queue state: a queueReasons key
// carrying the value its field writes.
func isQueueState(reason string) bool {
	key, value, found := strings.Cut(reason, "=")
	accept, known := queueReasons[strings.ToLower(key)]
	return found && known && accept(value)
}

// healthLineHead reads a health line's shape: one of the four verdicts the town
// writes, the "tick " marker, and a non-empty age before " ago". It returns the
// verdict and everything after " ago". ok is false for any other shape, so a
// line this does not know is refused rather than read.
func healthLineHead(line string) (verdict, tail string, ok bool) {
	verdict, rest, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found {
		return "", "", false
	}
	verdict = strings.ToUpper(verdict)
	switch verdict {
	case "GREEN", "DEGRADED", "RED", "UNKNOWN":
	default:
		return "", "", false
	}
	rest, found = strings.CutPrefix(rest, "tick ")
	if !found {
		return "", "", false
	}
	age, tail, found := strings.Cut(rest, " ago")
	if !found || age == "" {
		return "", "", false
	}
	return verdict, tail, true
}

// healthReasons splits a health line into its verdict and the reason tokens it
// lists: degraded is whether the verdict is DEGRADED, and tokens are the
// reasons after the colon, separated by commas or by spaces, so both the
// comma-separated form and the space-separated line the town renders read. ok
// is false for any shape this does not know — a RED or UNKNOWN verdict, a line
// without the "<VERDICT> tick <age> ago" head, a DEGRADED line with no reason.
func healthReasons(line string) (degraded bool, tokens []string, ok bool) {
	verdict, tail, ok := healthLineHead(line)
	if !ok {
		return false, nil, false
	}
	switch verdict {
	case "GREEN":
		return false, nil, true
	case "DEGRADED":
	default:
		return false, nil, false
	}
	reasons, found := strings.CutPrefix(tail, ":")
	if !found {
		return false, nil, false
	}
	tokens = strings.FieldsFunc(reasons, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(tokens) == 0 {
		return false, nil, false
	}
	return true, tokens, true
}

// healthClass is what the verify step makes of a health line: the town passed,
// it is still settling into the new daemon, or it has a fault.
type healthClass string

const (
	healthPass     healthClass = "pass"
	healthSettling healthClass = "settling"
	healthFail     healthClass = "fail"
)

// classifyHealthLine classifies line for the verify step. A GREEN line, and a
// DEGRADED one whose every reason is a queue state, pass as queueOnlyHealthLine
// reads them. A DEGRADED line whose every reason is a queue state, a RECORDED
// tick reason — "tick:<patrol>=<age>/<interval>[R]", a patrol whose last pass
// predates the restart and is late until its first pass finishes — or a
// RECORDED dead seat — "seat/<rig>:polecat/<name>=dead[R]", a polecat whose
// session is down while it is re-slung — settles, since a later read clears
// the reason or the bound names the seat. Everything else fails: another verdict, a
// tick or seat reason without the tag or with a value that is not the town's,
// another key, a shape this does not know.
func classifyHealthLine(line string) healthClass {
	degraded, tokens, ok := healthReasons(line)
	if !ok {
		return healthFail
	}
	if !degraded {
		return healthPass
	}
	settling := false
	for _, reason := range tokens {
		if isQueueState(reason) {
			continue
		}
		key, value, _ := strings.Cut(reason, "=")
		if !recordedTickReason(key, value) && !recordedDeadSeatReason(key, value) {
			return healthFail
		}
		settling = true
	}
	if settling {
		return healthSettling
	}
	return healthPass
}

// recordedTickReason reports whether key=value is a tick field as the town's
// line renders one, "tick:<patrol>=<age>/<interval>[R]": a patrol with a short
// duration either side of the slash, carrying the [R] of a RECORDED field.
func recordedTickReason(key, value string) bool {
	patrol, found := strings.CutPrefix(key, "tick:")
	value, tagged := strings.CutSuffix(value, "[R]")
	age, interval, shaped := strings.Cut(value, "/")
	return found && tagged && shaped && patrol != "" &&
		isShortDuration(age) && isShortDuration(interval)
}

// recordedDeadSeatReason reports whether key=value is a polecat seat dead in
// consecutive samples as the town's line renders it,
// "seat/<rig>:polecat/<name>=dead[R]": the value is exactly dead with the [R]
// of a RECORDED field, and the key names a rig and a polecat. A seat frozen or
// stalled, a dead seat without the tag, and any other seat kind do not match.
func recordedDeadSeatReason(key, value string) bool {
	seat, found := strings.CutPrefix(key, "seat/")
	rig, name, shaped := strings.Cut(seat, ":polecat/")
	return found && value == "dead[R]" && shaped && rig != "" && name != ""
}

// isShortDuration reports whether s is a duration as internal/townhealth.Short
// renders it: a count and one unit, such as 400ms, 28s, 5m, 2h or 3d.
func isShortDuration(s string) bool {
	for _, unit := range []string{"ms", "s", "m", "h", "d"} {
		if count, found := strings.CutSuffix(s, unit); found {
			return isCount(count)
		}
	}
	return false
}

// isOldestAge reports whether s is the value the town's escalation field
// writes: "oldest" joined to the age of the oldest open escalation, which is a
// short duration with a single unit as internal/townhealth's Short renders it
// (400ms, 28s, 5m, 2h, 3d), the space between the two words compacted to an
// underscore. A count is not this value, and neither is an age without a unit.
func isOldestAge(s string) bool {
	age, found := strings.CutPrefix(s, "oldest_")
	if !found {
		return false
	}
	for _, unit := range []string{"ms", "s", "m", "h", "d"} {
		if count, found := strings.CutSuffix(age, unit); found {
			return isCount(count)
		}
	}
	return false
}

// isCount reports whether s is a whole-number count: one or more decimal
// digits, with no sign, space or tag marker. An empty value is not a count, so
// a queue reason without one fails rather than passes.
func isCount(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sameCommit reports whether two commit strings name the same build, allowing
// the short and full forms of one commit to match.
func sameCommit(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if len(a) < len(b) {
		a, b = b, a
	}
	return len(b) >= shortCommitLen && strings.HasPrefix(a, b)
}
