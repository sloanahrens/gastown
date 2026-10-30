package forgejoresync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/openshellprov"
)

// restoreTokenEnv is the variable the provisioner reads the admin token from before
// any file. The restore hands the token over through it in memory, so the token
// never reaches argv.
const restoreTokenEnv = "GT_AUTH_FORGEJO_TOKEN" //nolint:gosec // G101: the variable's name, not a credential

// defaultRestoreTimeout bounds the provisioner run that restores protection. The restore
// is a short run of HTTP calls, so two minutes is ample; the bound only ends a hung connection.
const defaultRestoreTimeout = 2 * time.Minute

// errRestoreFailed is the provisioner's refusal to restore. Its text names no output,
// since the provisioner's own lines already reached the log.
var errRestoreFailed = errors.New("provisioner run failed")

// Step is one ordered stage of the run. An error from a stage stops the run there, and
// the protection restore still runs.
type Step struct {
	Name string
	Run  func() error
}

// restore re-runs the provisioner in process with the options a cutover used, so it
// recreates main's rule. Under a dry run it sends nothing and logs what it would do.
// The run is bounded by its own deadline, derived from the background context, so a
// failure earlier in the run does not cancel the restore.
func (r *Resync) restore(token forgejoprov.Secret) error {
	if r.Opts.DryRun {
		r.logf("+ restore %s protection on %s (dry run: not run)", r.Opts.MainBranch, r.Opts.Repo)
		return nil
	}
	opts, err := forgejoprov.Parse([]string{
		"--repo", r.Opts.Repo,
		"--main-branch", r.Opts.MainBranch,
		"--api-url", r.Opts.APIURL,
	}, r.getenv())
	if err != nil {
		return err
	}
	// The provisioner's output reaches the run's log through two writers. The token is
	// masked first, across writes; the masked stream is then split into lines. Closing
	// the masker flushes any partial token it still holds, so that tail is masked and
	// logged before the by-hand message.
	lines := &logLines{log: func(line string) { r.logf("%s", token.Scrub(line)) }}
	masked := openshellprov.Masked(token.Reveal(), lines)
	defer func() {
		// masked fails only when lines does, and lines never fails, so Close cannot.
		_ = masked.Close()
		lines.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), r.restoreTimeout())
	defer cancel()
	env := forgejoprov.Env{
		Getenv: func(name string) string {
			if name == restoreTokenEnv {
				return token.Reveal()
			}
			return ""
		},
		Log: masked,
	}
	if r.Client != nil {
		env.HTTP = r.Client.HTTP
	}
	provision := r.Provision
	if provision == nil {
		provision = forgejoprov.Run
	}
	if provision(ctx, opts, env) != forgejoprov.ExitOK {
		return errRestoreFailed
	}
	return nil
}

// restoreTimeout returns the bound on the restore: RestoreTimeout when it is set, else
// the default.
func (r *Resync) restoreTimeout() time.Duration {
	if r.RestoreTimeout > 0 {
		return r.RestoreTimeout
	}
	return defaultRestoreTimeout
}

// logLines turns the provisioner's output into log lines. A line is logged when its
// newline arrives, and Close logs an unterminated last line.
type logLines struct {
	pending []byte
	log     func(string)
}

// Write buffers p and logs each line it completes.
func (w *logLines) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for i := bytes.IndexByte(w.pending, '\n'); i >= 0; i = bytes.IndexByte(w.pending, '\n') {
		w.log(string(w.pending[:i]))
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

// Close logs the unterminated last line, if one is buffered.
func (w *logLines) Close() {
	if len(w.pending) > 0 {
		w.log(string(w.pending))
		w.pending = nil
	}
}

// getenv returns the environment seam, or the process environment when none is set.
func (r *Resync) getenv() func(string) string {
	if r.Getenv != nil {
		return r.Getenv
	}
	return os.Getenv
}

// readFile returns the token-file seam, or the real file reader when none is set.
func (r *Resync) readFile() func(string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile
	}
	return os.ReadFile
}
