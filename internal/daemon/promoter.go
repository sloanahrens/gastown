package daemon

import (
	"os"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/promote"
)

// PromoteDivergedAlertKey is the alert key one rig's GitHub promotion
// divergence is raised under, whichever caller found it. The landing worker's
// verdict, the tier sweep and `gt promote` record the same condition on the
// same rig, so a second key would page a human twice for it (gt-wilev).
func PromoteDivergedAlertKey(rigName string) string {
	return "landing-promote-diverged:" + rigName
}

// NewRigPromoter builds a rig's GitHub promotion owner from the operator's
// promote_target and promote_key_file (merge_queue.forgejo, resolved before
// this call), over repo, the rig's own repository. One constructor for every
// caller is what keeps a rig to one push path, one lock and one record
// (gt-fn9e6.38).
//
// It returns nil for a rig that does not promote — no promote_target, or a
// target with no deploy key — so a caller has nothing to wire. The key path
// goes through promote.ResolveKeyFile, so a key mounted under XDG_CONFIG_HOME
// is used when the configured path is absent. escalate and logf may be nil.
func NewRigPromoter(townRoot, rigName string, fj *config.ForgejoConfig, repo promote.Repo, escalate func(message string), logf func(format string, args ...any)) *promote.Promoter {
	return newRigPromoter(townRoot, rigName, fj, repo, escalate, logf, os.Getenv, statPath)
}

// newRigPromoter is NewRigPromoter with the environment and file-existence
// reads injected, so a test answers XDG_CONFIG_HOME and stat from fakes.
func newRigPromoter(townRoot, rigName string, fj *config.ForgejoConfig, repo promote.Repo, escalate func(message string), logf func(format string, args ...any), getenv func(string) string, stat func(string) error) *promote.Promoter {
	if fj == nil || fj.PromoteTarget == "" {
		return nil
	}
	if fj.PromoteKeyFile == "" {
		if logf != nil {
			logf("promote: %s: merge_queue.forgejo.promote_target is set but promote_key_file is not, so GitHub promotion is off until the deploy key is named", rigName)
		}
		return nil
	}
	keyFile := promote.ResolveKeyFile(fj.PromoteKeyFile, getenv, stat, func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	})
	return &promote.Promoter{
		Rig:      rigName,
		Target:   fj.PromoteTarget,
		KeyFile:  keyFile,
		Repo:     repo,
		LockPath: promote.LockPath(townRoot, rigName),
		Escalate: escalate,
		Logf:     logf,
	}
}

// statPath is the file-existence seam promote.ResolveKeyFile reads through.
func statPath(path string) error {
	_, err := os.Stat(path)
	return err
}

// RedMainStateStore is the file holding a rig's red-main state
// (RedMainStatePath) as a store, so a writer outside this package — gt
// promote — records last_promoted in the same file and shape the landing
// worker's verdict and the tier sweep write.
func RedMainStateStore(townRoot, rigName string) landworker.MainStateStore {
	return fileMainState{path: RedMainStatePath(townRoot, rigName)}
}
