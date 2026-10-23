package session

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/liveness"
)

// The openshell implementation sets these two hooks when it is linked in.
// They are package variables rather than an injected object because the
// implementation is an optional link-time dependency of this package's
// callers (the daemon, gt sling, gt polecat), the same way NewOpenShellBackend
// is: every entry point that needs them takes an explicit function too
// (CleanupSandboxWith, ResolveWith), and tests inject through those.
var (
	// SandboxDeleter deletes the sandbox a record describes. Nil means the
	// record is removed but the sandbox is left for the operator.
	SandboxDeleter func(townRoot string, rec SandboxRecord) error
	// SandboxLiveness assesses a sandboxed session where it lives (the
	// transcript is inside the sandbox). Nil leaves a sandboxed session's
	// verdict Unknown: the host probe's transcript read would not see it.
	SandboxLiveness func(townRoot string, rec SandboxRecord, in liveness.Input) liveness.Result
)

// CleanupSandbox removes what a sandboxed session left: the sandbox itself
// through SandboxDeleter, then its registry record and routed client. A
// session without a record is a host session and nothing happens. Callers
// run it after the session's processes were killed (the kill is routed).
func CleanupSandbox(townRoot, session string) error {
	return CleanupSandboxWith(SandboxDeleter, townRoot, session)
}

// CleanupSandboxWith is CleanupSandbox with the deleter given (nil = leave
// the sandbox, drop the record); CleanupSandbox passes SandboxDeleter.
func CleanupSandboxWith(deleter func(townRoot string, rec SandboxRecord) error, townRoot, session string) error {
	rec, ok, err := ReadSandboxRecord(townRoot, session)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if deleter != nil {
		if err := deleter(townRoot, rec); err != nil {
			return fmt.Errorf("deleting sandbox %s: %w", session, err)
		}
	}
	routing.forget(session)
	return RemoveSandboxRecord(townRoot, session)
}

// IsSandboxed reports whether session's record exists; a registry that
// cannot be read is an error, not "no".
func IsSandboxed(townRoot, session string) (bool, error) {
	_, ok, err := ReadSandboxRecord(townRoot, session)
	return ok, err
}

// AssessWhereItLives runs the liveness assessment for in.Session: through
// SandboxLiveness when the session is sandboxed and the hook is set, else
// through host (whose tmux probes route to the sandbox on their own).
func AssessWhereItLives(townRoot string, host liveness.Probe, in liveness.Input) liveness.Result {
	return assessWhereItLivesWith(SandboxLiveness, townRoot, host, in)
}

func assessWhereItLivesWith(sandboxed func(string, SandboxRecord, liveness.Input) liveness.Result, townRoot string, host liveness.Probe, in liveness.Input) liveness.Result {
	rec, ok, err := ReadSandboxRecord(townRoot, in.Session)
	if err != nil {
		// Not answered: a registry that cannot be read must not turn into a
		// host verdict on a session that may live in a sandbox.
		return liveness.Result{Verdict: liveness.Unknown, Reason: "sandbox registry unreadable", Err: err}
	}
	if ok {
		if sandboxed == nil {
			return liveness.Result{Verdict: liveness.Unknown, Reason: "sandboxed session, no sandbox liveness linked"}
		}
		return sandboxed(townRoot, rec, in)
	}
	return liveness.Assess(host, in)
}
