package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// SandboxRecord says that a polecat's session lives in an OpenShell sandbox.
// One file per session under <town>/.runtime/sandboxes/<session>.json,
// written by the openshell backend's Prepare and removed by its Delete. The
// tmux router reads it (sandboxRouting below) so any client in any gt
// process reaches the session; the daemon's listings count it as a live
// session. It is durable on purpose: a daemon restart must still route to
// a sandbox that is running.
type SandboxRecord struct {
	Session   string    `json:"session"`
	Rig       string    `json:"rig"`
	Polecat   string    `json:"polecat"`
	Gateway   string    `json:"gateway,omitempty"`
	Profile   string    `json:"profile"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	// Branch and WorkDir locate the clone inside the sandbox, so a stop can
	// push its commits before the sandbox is deleted (gt-hwpfq). Empty on a
	// record written before that push existed.
	Branch  string `json:"branch,omitempty"`
	WorkDir string `json:"work_dir,omitempty"`
}

// SandboxRegistryDir is where the records live.
func SandboxRegistryDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "sandboxes")
}

func sandboxRecordPath(townRoot, session string) string {
	return filepath.Join(SandboxRegistryDir(townRoot), session+".json")
}

// WriteSandboxRecord records r for its session.
func WriteSandboxRecord(townRoot string, r SandboxRecord) error {
	if !validSessionName(r.Session) {
		return fmt.Errorf("sandbox record: bad session name %q", r.Session)
	}
	if err := os.MkdirAll(SandboxRegistryDir(townRoot), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(sandboxRecordPath(townRoot, r.Session), append(data, '\n'), 0o644)
}

// ReadSandboxRecord returns the record for session; ok is false when there
// is none (the session is a host tmux session, or nothing).
func ReadSandboxRecord(townRoot, session string) (r SandboxRecord, ok bool, err error) {
	if !validSessionName(session) {
		return r, false, nil
	}
	data, err := os.ReadFile(sandboxRecordPath(townRoot, session))
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, false, fmt.Errorf("sandbox record %s: %w", session, err)
	}
	return r, true, nil
}

// RemoveSandboxRecord forgets session. Idempotent.
func RemoveSandboxRecord(townRoot, session string) error {
	if !validSessionName(session) {
		return nil
	}
	err := os.Remove(sandboxRecordPath(townRoot, session))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ListSandboxSessions returns the recorded session names, sorted; a
// missing or unreadable registry lists nothing.
func ListSandboxSessions(townRoot string) []string {
	out, _ := listSandboxSessions(townRoot)
	return out
}

// listSandboxSessions is ListSandboxSessions reporting a read error; a
// missing directory is an empty list, not an error.
func listSandboxSessions(townRoot string) ([]string, error) {
	entries, err := os.ReadDir(SandboxRegistryDir(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// sessionNameRe is the shape of a session name that may name a record
// file: a tmux session name as PrefixRegistry forms them, never a path
// element, never "." or "..", never hidden.
var sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validSessionName(session string) bool {
	return session != "." && session != ".." && sessionNameRe.MatchString(session)
}

// SandboxExecPrefix is the wrapper argv that runs a command inside the
// sandbox of session on gateway: the exec prefix a routed tmux client uses.
func SandboxExecPrefix(gateway, session string) []string {
	argv := []string{"openshell"}
	if gateway != "" {
		argv = append(argv, "-g", gateway)
	}
	return append(argv, "sandbox", "exec", "-n", session, "--no-tty", "--no-login-shell", "--")
}

// SandboxTmuxSocket is the tmux socket name of the server inside a sandbox.
const SandboxTmuxSocket = "gt"
