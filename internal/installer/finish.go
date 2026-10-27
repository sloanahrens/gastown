package installer

import "strings"

// Syncs runs the two convenience syncs after a good install, as rebuild-gt
// does: neither can fail the install, so a failure is one log line and the run
// carries on.
func (r *Run) Syncs() {
	for _, sync := range []string{"formula", "plugin"} {
		out, errOut, code := r.Cmd.run(r.Paths.RigDir, r.Paths.GT(), sync, "sync")
		said := strings.TrimRight(string(out)+string(errOut), "\n")
		if code == 0 {
			r.C.Log(said)
			continue
		}
		r.C.Log(sync + " sync failed (non-fatal): " + said)
	}
}

// Finalize is the script's "Restart marker, receipt" section: the daemon is
// told to restart into the installed commit, the receipt records the install,
// and the fingerprints this install proves stale are cleared.
//
// The binary is installed and smoke-verified by the time this runs, so a marker
// that cannot be written is not a build, install or smoke failure and must
// never roll a good binary back. It is its own reason (marker-write): the
// receipt and the escalation say plainly that the binary is in force but the
// daemon was not told to restart into it.
func (r *Run) Finalize() Exit {
	expected, prev := r.C.Expected, r.C.Prev
	if err := r.WriteRestartMarker(expected); err != nil {
		r.C.Escalate("high", "install-gt:marker-write-failed", expected+" is installed and verified at "+r.Paths.GT()+
			", but writing "+r.Paths.Marker()+" failed — the binary stays installed; the daemon needs a manual restart to pick it up")
		r.C.Receipt("failed", expected, prev, "marker-write")
		return r.C.Finish(Result{Event: "failed", Commit: expected, Prev: prev, Reason: "marker-write"}, ExitFailed)
	}
	r.C.Receipt("installed", expected, prev, "")
	r.clearEscalations()
	// --no-image-first rebuilds the sandbox image here, detached and best
	// effort, once the daemon has been told to restart; that step is
	// gt-tpg0p.12.
	r.C.Log("In force: " + unknownIfEmpty(prev) + " -> " + expected + ". The daemon restarts itself when idle.")
	return r.C.Finish(Result{Event: "installed", Commit: expected, Prev: prev}, ExitOK)
}

// clearEscalations clears the two fingerprints this install proves stale,
// through whatever gt is installed. Output and status are discarded, as the
// script discards them: an install never depends on the town hearing about it.
func (r *Run) clearEscalations() {
	_, _, _ = r.Cmd.run("", r.Paths.GT(), "escalate", "clear",
		"--fingerprint", "install-gt:build-failed",
		"--fingerprint", "install-gt:smoke-failed",
		"--reason", "install-gt: "+r.C.Expected+" is in force")
}

// unknownIfEmpty is the script's ${PREV:-unknown}.
func unknownIfEmpty(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
