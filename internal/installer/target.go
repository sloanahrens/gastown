package installer

import "path/filepath"

// Target is the commit a run installs and the one it replaces: what the no-op
// check and the refusals decide about.
type Target struct {
	// FullSHA is the commit --sha resolved to, after the fetch.
	FullSHA string
	// Prev is the commit the installed binary was built from, resolved; empty
	// when the binary says nothing or the checkout lacks what it named.
	Prev string
}

// ResolveTarget is the script's "Resolve the target" section. It ends the run
// when there is no build checkout (failed/no-rig) or the commit is unknown
// (refused/unknown-commit); a git read that fails for no reason of its own ends
// it the way the script's set -e does, with the guard's failed/unexpected.
func (r *Run) ResolveTarget() (Exit, bool) {
	if !r.Files.exists(filepath.Join(r.Paths.RigDir, ".git")) {
		r.C.Log("No build checkout at " + r.Paths.RigDir)
		r.C.Receipt("failed", r.C.Expected, r.C.Prev, "no-rig")
		return r.C.Finish(Result{Event: "failed", Commit: r.C.Expected, Reason: "no-rig"}, ExitFailed), true
	}
	if err := r.Repo.Fetch(); err != nil {
		r.C.Log("WARNING: fetch failed; using local refs")
	}
	fullSHA := r.Repo.Resolve(r.Cfg.SHA)
	if fullSHA == "" {
		return r.refuse("unknown-commit", r.Cfg.SHA+" is not a commit in "+r.Paths.RigDir)
	}
	r.C.Expected = fullSHA
	mergedAt, err := r.Repo.CommitTime(fullSHA)
	if err != nil {
		return ExitFailed, true
	}
	prev := r.Repo.Resolve(r.Bin.Commit(r.Paths.RigDir, r.Paths.GT()))
	r.Target = Target{FullSHA: fullSHA, Prev: prev}
	r.C.Prev = prev
	r.C.MergedAt = mergedAt
	return ExitOK, false
}

// Refuse is the script's "Refusals" section, the checks that keep a build from
// installing something local work has made unrepresentative of main. The
// fast-forward is the one that moves the checkout: a target the local main
// cannot reach without a reset is a human's call (gt-4g1m).
func (r *Run) Refuse() (Exit, bool) {
	if r.Repo.Dirty() {
		return r.refuse("dirty", r.Paths.RigDir+" has uncommitted changes")
	}
	if branch := r.Repo.Branch(); branch != "main" {
		return r.refuse("wrong-branch", r.Paths.RigDir+" is on '"+branch+"', not main")
	}
	if err := r.Repo.FastForward(r.Target.FullSHA); err != nil {
		return r.refuse("diverged", "local main cannot fast-forward to "+r.Target.FullSHA)
	}
	if r.Repo.AheadOf("origin/main") > 0 {
		return r.refuse("diverged", "local main has commits origin/main lacks")
	}
	head, err := r.Repo.Head()
	if err != nil {
		return ExitFailed, true
	}
	if r.Target.Prev != "" && !r.Repo.IsAncestor(r.Target.Prev, head) {
		return r.refuse("not-forward", "installed "+r.Target.Prev+" is not an ancestor of "+head+" (would be a downgrade)")
	}
	// HEAD can be past FullSHA when the checkout was already on a newer main;
	// that is forward and contains the commit, so HEAD is what gets built.
	r.C.Expected = head
	return ExitOK, false
}

// refuse is the script's refuse(): the log line, the refused receipt and result
// line, and exit 2 with the binary untouched. The commit named is the target
// when it resolved, else the argument that did not.
func (r *Run) refuse(reason, message string) (Exit, bool) {
	commit := r.Target.FullSHA
	if commit == "" {
		commit = r.Cfg.SHA
	}
	r.C.Log("Refused: " + message)
	r.C.Receipt("refused", commit, r.C.Prev, reason)
	return r.C.Finish(Result{Event: "refused", Commit: commit, Prev: r.C.Prev, Reason: reason}, ExitRefused), true
}
