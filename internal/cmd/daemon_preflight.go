package cmd

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/daemon"
)

// PreflightResult is one pre-roll check's outcome: the check's name and the
// error it failed with, nil when it passed.
type PreflightResult struct {
	Name string
	Err  error
}

// The names the preflight reports for its checks.
const (
	preflightNameTownConfig  = "town-config"
	preflightNameBDHandshake = "bd-handshake"
)

// preflightChecks are the pre-roll checks, as fields so a unit test replaces
// them and runs neither a town config load nor bd.
type preflightChecks struct {
	// townConfig reports every town config file that exists but does not
	// load (daemon.CheckTownConfig).
	townConfig func(townRoot string) error
	// bdHandshake is the startup handshake for the town's store
	// (bdTownHandshake).
	bdHandshake func(ctx context.Context, townRoot string) error
}

// townPreflightChecks are the checks runPreflightChecks runs in production.
var townPreflightChecks = preflightChecks{
	townConfig:  daemon.CheckTownConfig,
	bdHandshake: bdTownHandshake,
}

// runPreflightChecks runs every pre-roll check against townRoot and returns
// one result per check, in run order: a failing check never stops the ones
// after it, so an operator sees every problem in one pass.
func runPreflightChecks(ctx context.Context, townRoot string) []PreflightResult {
	return runPreflightChecksWith(ctx, townRoot, townPreflightChecks)
}

// runPreflightChecksWith is runPreflightChecks with the checks passed in, so a
// test drives every result with no town on disk and no bd. The rows below are
// the order the checks run in, and a check with no function wired is reported
// as failed rather than passing silently.
func runPreflightChecksWith(ctx context.Context, townRoot string, checks preflightChecks) []PreflightResult {
	ordered := []struct {
		name  string
		wired bool
		run   func() error
	}{
		{preflightNameTownConfig, checks.townConfig != nil, func() error { return checks.townConfig(townRoot) }},
		{preflightNameBDHandshake, checks.bdHandshake != nil, func() error { return checks.bdHandshake(ctx, townRoot) }},
	}
	results := make([]PreflightResult, 0, len(ordered))
	for _, check := range ordered {
		var err error
		if check.wired {
			err = check.run()
		} else {
			err = fmt.Errorf("the preflight has no %s check wired", check.name)
		}
		results = append(results, PreflightResult{Name: check.name, Err: err})
	}
	return results
}
