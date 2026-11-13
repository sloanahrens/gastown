package daemon

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/buildsrc"
	"github.com/steveyegge/gastown/internal/buildstamp"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/version"
)

// startBuildCheckout ensures the gastown rig's build checkout at the full commit
// this daemon was built from, and refuses the start when that fails or when a
// configured sandbox profile's policy file is missing from it. It runs only in a
// pod (container mode): a host daemon keeps the checkout it has. The rev is
// never empty: Ensure("") follows the bare repo's main, which a pod does not
// keep current, so a build commit that is not a full sha is a refusal, not a
// fallback.
func (d *Daemon) startBuildCheckout(ctx context.Context) error {
	if !d.containerMode {
		d.logger.Printf("the gt source checkout is not ensured: a host daemon keeps the checkout it has")
		return nil
	}
	sha, err := d.fullBuildCommit()
	if err != nil {
		return fmt.Errorf("refusing to start: the daemon's build commit is not known in full: %w", err)
	}
	if _, err := buildstamp.Short(sha); err != nil {
		return fmt.Errorf("refusing to start: the daemon's build commit is not a full sha: %w", err)
	}

	ensure := d.buildCheckoutEnsureFn
	if ensure == nil {
		ensure = buildsrc.Ensure
	}
	checkout, err := ensure(ctx, d.config.TownRoot, rebuildGTSourceRig, sha)
	if err != nil {
		return fmt.Errorf("refusing to start: the gt source checkout at %s: %w", sha, err)
	}
	d.logger.Printf("the gt source checkout is %s at %s", checkout, sha)

	ts, err := agentconfig.LoadTownSettings(agentconfig.TownSettingsPath(d.config.TownRoot))
	if err != nil {
		return fmt.Errorf("refusing to start: reading the town settings for the sandbox policies: %w", err)
	}
	if ts == nil {
		return nil
	}
	if err := session.CheckPolicyFiles(checkout, ts.Sandboxes); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	return nil
}

// fullBuildCommit is the daemon's full build commit: fullBuildCommitFn's when a
// test set one, else the version package's.
func (d *Daemon) fullBuildCommit() (string, error) {
	if d.fullBuildCommitFn != nil {
		return d.fullBuildCommitFn()
	}
	return version.FullBuildCommit()
}
