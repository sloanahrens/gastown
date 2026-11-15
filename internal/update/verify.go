package update

import (
	"context"
	"fmt"
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

// WaitForRestart polls the daemon until it reports want or timeout passes,
// which is measured from the first poll. The poll that finds the timeout passed
// is the last one, so a sleep that ends on the timeout still gets its poll; a
// canceled context or a failing sleep returns that error.
func WaitForRestart(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p VerifyProbes, want string, timeout, poll time.Duration) (bool, error) {
	start := clock.Now()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// A probe error is expected while the daemon is down mid-restart, so
		// only a version that matches ends the wait.
		if version, err := p.DaemonVersion(ctx); err == nil && sameCommit(version, want) {
			return true, nil
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
// and its escalation field — and a reason outside them fails.
var queueReasons = map[string]func(string) bool{
	"needs-overseer": isCount,
	"needs-operator": isCount,
	"needs-human":    isCount,
	"escalation":     isOldestAge,
}

// healthCheck verifies the install, not the town's queue: it passes on a GREEN
// line and on a DEGRADED line whose only reasons are queue states, naming the
// reasons it ignored. Any other verdict, a DEGRADED line with any other reason
// or with no reason, and a line that does not read
// "<VERDICT> tick <age> ago" fails, and the detail carries the line as read. A
// probe error fails the check with the probe's message.
func healthCheck(ctx context.Context, p VerifyProbes) Check {
	line, err := p.Health(ctx)
	if err != nil {
		return Check{Name: checkHealth, Detail: err.Error()}
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
	verdict, rest, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found {
		return nil, false
	}
	degraded := false
	switch strings.ToUpper(verdict) {
	case "GREEN":
	case "DEGRADED":
		degraded = true
	default:
		return nil, false
	}
	rest, found = strings.CutPrefix(rest, "tick ")
	if !found {
		return nil, false
	}
	age, tail, found := strings.Cut(rest, " ago")
	if !found || age == "" {
		return nil, false
	}
	if !degraded {
		return nil, true
	}
	reasons, found := strings.CutPrefix(tail, ":")
	if !found {
		return nil, false
	}
	tokens := strings.FieldsFunc(reasons, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(tokens) == 0 {
		return nil, false
	}
	for _, reason := range tokens {
		key, value, found := strings.Cut(reason, "=")
		accept, known := queueReasons[strings.ToLower(key)]
		if !found || !known || !accept(value) {
			return nil, false
		}
		queue = append(queue, reason)
	}
	return queue, true
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
