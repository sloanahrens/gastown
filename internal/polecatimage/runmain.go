package polecatimage

// maxRounds is how many builds Main makes before it gives up on an install that
// keeps moving, the script's MAX_ROUNDS.
const maxRounds = 3

// Main keeps the image at the installed gt's commit, publishing each build when
// a registry is named, and answers the failure it cannot carry past.
func (r *Run) Main() error {
	installed := r.InstalledCommit()
	if installed == "" {
		return r.failf("cannot read the installed gt's commit from '%s version'", r.Cfg.GT)
	}
	// Docker unavailable is a state to wait out, not a failure to raise every
	// cooldown: without the check an unreachable docker reads as "no image" and
	// the build then fails.
	if r.call(r.Cfg.Docker, "info").failed() {
		return r.Defer("deferred: docker unavailable")
	}
	if SameCommit(r.ImageCommit(), installed) {
		if err := r.Republish(); err != nil {
			return err
		}
		r.Logf("current at %s", shortCommit(installed))
		return nil
	}
	if reason := r.DeferReason(); reason != "" {
		return r.Defer("deferred: " + reason)
	}
	why, err := r.BuildLock()
	if err != nil {
		return err
	}
	if why != "" {
		return r.DeferLockWait(why)
	}
	for i := 0; i < maxRounds; i++ {
		installed = r.InstalledCommit()
		if installed == "" {
			return r.failf("cannot read the installed gt's commit from '%s version'", r.Cfg.GT)
		}
		if SameCommit(r.ImageCommit(), installed) {
			r.Logf("current at %s", shortCommit(installed))
			return nil
		}
		// A sling can start between rounds, and a build is long.
		if reason := r.DeferReason(); reason != "" {
			return r.Defer("deferred: " + reason)
		}
		if err := r.BuildAt(installed); err != nil {
			return err
		}
		if err := r.PushToRegistry(); err != nil {
			return err
		}
	}
	installed = r.InstalledCommit()
	if SameCommit(r.ImageCommit(), installed) {
		r.Logf("current at %s", shortCommit(installed))
		return nil
	}
	return r.failf("the installed gt kept moving: %s is still not at %s after %d builds", r.Cfg.Image, shortCommit(installed), maxRounds)
}
