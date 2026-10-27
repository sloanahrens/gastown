package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Noop is the script's "No-op" section: when the installed binary already
// contains the target, nothing is built, and the daemon is told to restart only
// when it is not already running a build that contains the target too. The
// comparison is ancestry, not equality: a daemon ahead of the target has
// nothing to restart for (gt-tg5to).
func (r *Run) Noop() (Exit, bool) {
	fullSHA, prev := r.Target.FullSHA, r.Target.Prev
	if prev == "" || !r.Repo.IsAncestor(fullSHA, prev) {
		return ExitOK, false
	}
	recorded, ok := r.jsonCommit("state.json")
	if !ok {
		r.C.Log("Installed " + prev + " already contains " + fullSHA + "; nothing to do.")
	} else if daemon := r.Repo.Resolve(recorded); daemon == "" {
		// The daemon names a commit the checkout does not have, so its build
		// cannot be compared with the target. That is not evidence it already
		// holds the target, so the run installs rather than reporting a no-op.
		r.C.Log("WARNING: the daemon's commit " + recorded + " is not in " + r.Paths.RigDir + "; installing rather than leaving the daemon on an unknown build.")
		return ExitOK, false
	} else if !r.Repo.IsAncestor(fullSHA, daemon) {
		pending := r.resolve(r.pendingCommit())
		if pending != "" && r.Repo.IsAncestor(fullSHA, pending) {
			r.C.Log("A pending restart marker for " + pending + " already contains " + fullSHA + "; leaving it in place.")
		} else {
			r.C.Log("Installed " + prev + " already contains " + fullSHA + ", but the daemon is running " + daemon + "; writing the restart marker.")
			if err := r.WriteRestartMarker(fullSHA); err != nil {
				r.C.Escalate("high", "install-gt:marker-write-failed", fullSHA+" is installed at "+r.Paths.GT()+
					" and the running daemon is still on "+daemon+", but writing "+r.Paths.Marker()+
					" failed — the binary stays installed; the daemon needs a manual restart to pick it up")
				r.C.Receipt("failed", fullSHA, prev, "marker-write")
				return r.C.Finish(Result{Event: "failed", Commit: fullSHA, Prev: prev, Reason: "marker-write"}, ExitFailed), true
			}
		}
	} else {
		r.C.Log("Installed " + prev + " already contains " + fullSHA + "; nothing to do.")
	}
	// The script runs the gateway step on this path too, before the receipt; it
	// reports its own failures and never changes the outcome (gt-tpg0p.9).
	r.GatewayStep(GatewayInstall)
	r.C.Receipt("noop", fullSHA, prev, "already-installed")
	return r.C.Finish(Result{Event: "noop", Commit: fullSHA, Prev: prev, Reason: "already-installed"}, ExitOK), true
}

// WriteRestartMarker writes the marker the daemon restarts into for commit.
// The write is a temporary file renamed into place, so the daemon reads the old
// marker or the new one and never half of either (gt-tqxdd).
func (r *Run) WriteRestartMarker(commit string) error {
	repo, err := filepath.Abs(r.Paths.RigDir)
	if err != nil {
		repo = r.Paths.RigDir
	}
	// Sorted keys and whole seconds keep the line identical to the one the
	// script wrote, until the script goes (gt-tpg0p.14).
	marker := map[string]string{
		"commit":       commit,
		"requested_at": r.C.now().UTC().Format("2006-01-02T15:04:05Z"),
		"repo":         repo,
		"source":       r.C.Source,
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	path := r.Paths.Marker()
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := r.Files.writeFile(tmp, data, 0o666); err != nil {
		return err
	}
	if err := r.Files.rename(tmp, path); err != nil {
		_ = r.Files.remove(tmp)
		return err
	}
	return nil
}

// jsonCommit is the "commit" field of name in the daemon directory. ok is false
// when the file is missing, unreadable, not JSON, or names no commit, which is
// a daemon that recorded nothing rather than one on an unknown build.
func (r *Run) jsonCommit(name string) (commit string, ok bool) {
	data, err := r.Files.readFile(filepath.Join(r.Paths.DaemonDir, name))
	if err != nil {
		return "", false
	}
	var record struct {
		Commit string `json:"commit"`
	}
	if json.Unmarshal(data, &record) != nil || record.Commit == "" {
		return "", false
	}
	return record.Commit, true
}

// pendingCommit is the commit an earlier install left in the restart marker.
func (r *Run) pendingCommit() string {
	commit, _ := r.jsonCommit("restart-pending.json")
	return commit
}

// resolve is a commit the daemon recorded as the checkout's full hash, or ""
// when it recorded nothing or the checkout does not have it.
func (r *Run) resolve(ref string) string {
	if ref == "" {
		return ""
	}
	return r.Repo.Resolve(ref)
}
