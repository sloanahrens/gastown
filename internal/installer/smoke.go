package installer

import "encoding/json"

// Smoke is the script's smoke check: the gt the town will execute is the commit
// that was built. It runs the installed binary and ends the run, rolled back,
// when it cannot answer 'gt stale --json' or answers a commit other than the
// expected one. A binary that fails here is the one failure worth undoing an
// install for, which is why it is a high escalation and a rollback.
func (r *Run) Smoke() (Exit, bool) {
	gt := r.Paths.GT()
	// stderr is discarded, as the script's `2>/dev/null`: only stdout has to be
	// the JSON.
	stdout, _, code := r.Cmd.run(r.Paths.RigDir, gt, "stale", "--json")
	if code != 0 || !json.Valid(stdout) {
		return r.FailInstall("smoke-failed", "the new binary cannot answer 'gt stale --json'",
			"high", "install-gt:smoke-failed", "rolled_back"), true
	}
	raw := r.Bin.Commit(r.Paths.RigDir, gt)
	got := r.Repo.Resolve(raw)
	if got == "" {
		read := raw
		if read == "" {
			read = "nothing"
		}
		return r.FailInstall("smoke-failed",
			"installed a build but cannot verify what came into force (read '"+read+"')",
			"high", "install-gt:smoke-failed", "rolled_back"), true
	}
	if got != r.C.Expected {
		return r.FailInstall("smoke-failed",
			"the install did not take — "+gt+" reports "+got+", built "+r.C.Expected,
			"high", "install-gt:smoke-failed", "rolled_back"), true
	}
	return ExitOK, false
}
