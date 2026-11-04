package update

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	checkGTVersion     = "gt-version"
	checkDaemonVersion = "daemon-version"
	checkImageLabel    = "image-label"
	checkHealth        = "health"
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

// Check is one result of Verify: the fact checked, whether it matched, and
// what the probe reported.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Verify asks every probe what the town is running and reports four checks in
// a fixed order; a probe error fails its own check and leaves the others
// evaluated.
func Verify(ctx context.Context, p VerifyProbes, want string) []Check {
	return []Check{
		versionCheck(ctx, checkGTVersion, p.GTVersion, want),
		versionCheck(ctx, checkDaemonVersion, p.DaemonVersion, want),
		versionCheck(ctx, checkImageLabel, p.ImageLabel, want),
		healthCheck(ctx, p),
	}
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

// healthCheck passes when the town reports itself green.
func healthCheck(ctx context.Context, p VerifyProbes) Check {
	line, err := p.Health(ctx)
	if err != nil {
		return Check{Name: checkHealth, Detail: err.Error()}
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "green") {
		return Check{Name: checkHealth, Detail: line}
	}
	return Check{Name: checkHealth, OK: true, Detail: line}
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
