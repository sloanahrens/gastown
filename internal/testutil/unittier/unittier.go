// Package unittier checks, around a package's unit tests, the unit tier's
// rules that only a run can see (docs/testing.md, "The rules"):
//
//   - no goroutine a test started may still be running when the tests end;
//   - no test starts an external tool (refusedTools) found on PATH, whether
//     the test runs it or the code under test does.
//
// A package whose TestMain runs testutil.HermeticMain gets the checks from
// the harness. Every other package's unit-tier TestMain calls Main:
//
//	func TestMain(m *testing.M) {
//		os.Exit(unittier.Main(m))
//	}
//
// The package imports nothing from the module, so any package can use it,
// including the ones testutil itself imports. TestUnitTierMain in
// internal/testpolicy holds every package to one or the other. The checks are
// off in the integration tier (the integration build tag) and in a run that
// opted in to container tests (GT_TEST_DOCKER=1), whose tests drive real
// clients.
package unittier

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// Main runs the tests and returns the exit code for os.Exit: m.Run's code,
// forced to 1 when Check finds a broken rule.
func Main(m *testing.M, opts ...Option) int {
	r, err := Start(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unittier: %v\n", err)
		return 1
	}
	return r.Check(m.Run(), os.Stderr)
}

// Option configures Start.
type Option func(*config)

type config struct {
	allowed map[string]bool
}

// AllowTools lets the package's unit tier start the named refusedTools.
// It is the baseline of packages that still do: the list of names passed to
// it across the tree only shrinks (maxAllowedTools in internal/testpolicy).
func AllowTools(names ...string) Option {
	return func(c *config) {
		for _, n := range names {
			c.allowed[n] = true
		}
	}
}

// Run is the state Start takes before the tests and Check reads after them.
// A nil *Run checks nothing.
type Run struct {
	// goroutines were running when the tests started.
	goroutines map[int]bool
	// toolDir holds the refusing tools; "" when none were installed.
	toolDir string
}

// dockerTestsEnv is testutil.DockerTestsEnv, the opt-in to container tests.
const dockerTestsEnv = "GT_TEST_DOCKER"

// Start prepares the checks: it puts a refusing stand-in for each refusedTools
// entry not allowed by opts first on PATH, and then records the goroutines
// running, which Check does not blame on a test. Call it from TestMain, after
// any setup that runs those tools itself.
func Start(opts ...Option) (*Run, error) {
	cfg := config{allowed: map[string]bool{}}
	for _, opt := range opts {
		opt(&cfg)
	}
	if !enforced || os.Getenv(dockerTestsEnv) == "1" {
		return nil, nil
	}
	r := &Run{}
	dir, err := installRefusingTools(cfg.allowed)
	if err != nil {
		return nil, err
	}
	r.toolDir = dir
	r.goroutines = snapshot()
	return r, nil
}

// Check reports to w every goroutine still running that Start did not see,
// after giving stopped ones settle to return, and every refused tool start,
// and returns code, forced to 1 from 0 when there was any.
func (r *Run) Check(code int, w io.Writer) int {
	if r == nil {
		return code
	}
	code = reportLeaks(code, leaked(allGoroutines, r.goroutines, settle, time.Sleep), w)
	code = reportRefused(code, r.toolDir, w)
	if r.toolDir != "" {
		_ = os.RemoveAll(r.toolDir)
	}
	return code
}

// settle is how long Check waits for goroutines a test stopped, but that have
// not returned yet, to exit. A package with no stray goroutine pays nothing:
// the first look returns.
const settle = 2 * time.Second

// snapshot is the ids of the goroutines running now.
func snapshot() map[int]bool {
	gs := allGoroutines()
	ids := make(map[int]bool, len(gs))
	for _, g := range gs {
		ids[g.id] = true
	}
	return ids
}
