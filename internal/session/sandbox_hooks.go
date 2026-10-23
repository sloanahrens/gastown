package session

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/liveness"
)

// The openshell implementation (plan Task 5b) sets these two hooks when it
// is linked in. They take the sandbox record, so the implementation needs no
// config to act on a session the registry already describes.
var (
	// SandboxDeleter deletes the sandbox a record describes. Nil means the
	// record is removed but the sandbox is left for the operator.
	SandboxDeleter func(townRoot string, rec SandboxRecord) error
	// SandboxLiveness assesses a sandboxed session where it lives (the
	// transcript is inside the sandbox). Nil falls back to the host probe,
	// whose tmux calls are routed but whose transcript read is not.
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

// IsSandboxed reports whether session's record exists.
func IsSandboxed(townRoot, session string) bool {
	_, ok, _ := ReadSandboxRecord(townRoot, session)
	return ok
}

// AssessWhereItLives runs the liveness assessment for in.Session: through
// SandboxLiveness when the session is sandboxed and the hook is set, else
// through host (whose tmux probes route to the sandbox on their own).
func AssessWhereItLives(townRoot string, host liveness.Probe, in liveness.Input) liveness.Result {
	if rec, ok, _ := ReadSandboxRecord(townRoot, in.Session); ok && SandboxLiveness != nil {
		return SandboxLiveness(townRoot, rec, in)
	}
	return liveness.Assess(host, in)
}
