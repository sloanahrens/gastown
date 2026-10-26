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
	daemon := r.resolve(r.jsonCommit("state.json"))
	if daemon != "" && !r.Repo.IsAncestor(fullSHA, daemon) {
		pending := r.resolve(r.jsonCommit("restart-pending.json"))
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
	// The gateway step this path runs before its receipt lands in gt-tpg0p.9.
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

// jsonCommit is the "commit" field of name in the daemon directory, or "" when
// the file is missing, unreadable, not JSON, or names no commit.
func (r *Run) jsonCommit(name string) string {
	data, err := r.Files.readFile(filepath.Join(r.Paths.DaemonDir, name))
	if err != nil {
		return ""
	}
	var record struct {
		Commit string `json:"commit"`
	}
	if json.Unmarshal(data, &record) != nil {
		return ""
	}
	return record.Commit
}

// resolve is a commit the daemon recorded as the checkout's full hash, or ""
// when it recorded nothing or the checkout does not have it.
func (r *Run) resolve(ref string) string {
	if ref == "" {
		return ""
	}
	return r.Repo.Resolve(ref)
}
