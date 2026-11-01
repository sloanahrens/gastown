package polecatimage

import "os"

// maxRounds is how many builds Main makes before it gives up on an install that
// keeps moving, the script's MAX_ROUNDS.
const maxRounds = 3

// Main keeps the image at the installed gt's commit and the installed bd's
// commit, publishing each build when a registry is named, and answers the
// failure it cannot carry past.
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
	if current, bdUnread := r.current(installed); current {
		if err := r.Republish(); err != nil {
			return err
		}
		r.currentAt(installed, bdUnread)
		return nil
	}
	if reason := r.DeferReason(); reason != "" {
		return r.Defer("deferred: " + reason)
	}
	if imageBd, installedBd, pinned := r.bdPinned(installed); pinned {
		return r.failf("the image's bd is still at %s, not the installed %s, and the last build left it there: not building again until one of them moves", shortCommit(imageBd), shortCommit(installedBd))
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
		if current, bdUnread := r.current(installed); current {
			r.currentAt(installed, bdUnread)
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
		imageBd := r.ImageBdCommit()
		installedBd := r.InstalledBdCommit()
		if installedBd == "" || SameCommit(imageBd, installedBd) {
			r.currentAt(installed, installedBd == "")
			return nil
		}
		// The gt half is current and the bd label is still behind after the last
		// round. A Makefile BD_COMMIT pin keeps the label there. The run cannot
		// tell a pin from another cause, so it records the pair, and later runs
		// do not build for it again.
		r.recordBdPin(installedBd, imageBd)
		return r.failf("the image's bd is still at %s, not the installed %s after %d builds", shortCommit(imageBd), shortCommit(installedBd), maxRounds)
	}
	return r.failf("the installed gt kept moving: %s is still not at %s after %d builds", r.Cfg.Image, shortCommit(installed), maxRounds)
}

// current reports whether the image is current for this install: at the
// installed gt's commit and at the installed bd's commit, by the same prefix
// rule as SameCommit. The second result is true when the bd could not be read,
// so the bd half was not compared: a broken bd never loops the build.
func (r *Run) current(installed string) (current, bdUnread bool) {
	if !SameCommit(r.ImageCommit(), installed) {
		return false, false
	}
	installedBd := r.InstalledBdCommit()
	if installedBd == "" {
		return true, true
	}
	return SameCommit(r.ImageBdCommit(), installedBd), false
}

// currentAt ends a run whose image is current. A bd that could not be read is
// named in the line, since the image's bd was not compared with anything.
func (r *Run) currentAt(installed string, bdUnread bool) {
	r.clearBdPin()
	if bdUnread {
		r.Logf("current at %s; the installed bd was not read, so the image's bd was not checked", shortCommit(installed))
		return
	}
	r.Logf("current at %s", shortCommit(installed))
}

// bdPairRecord is what BdStuck holds for a pair of commits: the installed bd,
// then the image's bd, one line.
func bdPairRecord(installedBd, imageBd string) string {
	return installedBd + " " + imageBd + "\n"
}

// bdPinned reports whether the gt half is current, the bd label is behind the
// installed bd, and BdStuck names that same pair: the last run that ended stuck
// left the label there, so another build would leave it there too. A record
// that is missing or cannot be read is not a pin, so the run builds; a read
// error other than a missing record is logged.
func (r *Run) bdPinned(installed string) (imageBd, installedBd string, pinned bool) {
	if !SameCommit(r.ImageCommit(), installed) {
		return "", "", false
	}
	installedBd = r.InstalledBdCommit()
	imageBd = r.ImageBdCommit()
	if installedBd == "" || SameCommit(imageBd, installedBd) {
		return "", "", false
	}
	b, err := os.ReadFile(r.Cfg.BdStuck)
	if err != nil {
		if !os.IsNotExist(err) {
			r.Logf("cannot read %s: %v; building again", r.Cfg.BdStuck, err)
		}
		return "", "", false
	}
	return imageBd, installedBd, string(b) == bdPairRecord(installedBd, imageBd)
}

// recordBdPin writes BdStuck for the pair a stuck run ended with. A write that
// fails is logged, and the next run builds again.
func (r *Run) recordBdPin(installedBd, imageBd string) {
	if err := os.WriteFile(r.Cfg.BdStuck, []byte(bdPairRecord(installedBd, imageBd)), 0o644); err != nil {
		r.Logf("cannot write %s: %v; the next run builds again", r.Cfg.BdStuck, err)
	}
}

// clearBdPin removes BdStuck once the image is current, so no pair outlives the
// pin it recorded. A removal that fails is logged.
func (r *Run) clearBdPin() {
	if err := os.Remove(r.Cfg.BdStuck); err != nil && !os.IsNotExist(err) {
		r.Logf("cannot remove %s: %v", r.Cfg.BdStuck, err)
	}
}
