package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/hooks"
)

// Event types SyncSessionSettings' status is reported under: whether a
// starting session loads the managed guards (gt-be0z, gt-4k3fj.8.3).
const (
	EventHooksPresent = "hooks:present"
	EventHooksAbsent  = "hooks:absent"
)

// HooksStatus says whether a starting session's settings file carries the
// managed hooks gt hooks sync writes.
type HooksStatus struct {
	Present bool
	Role    string
	// Path is the settings file the session loads.
	Path string
	// Reason is why the guards are absent; "" when present.
	Reason string
}

// EventType is EventHooksPresent or EventHooksAbsent.
func (s HooksStatus) EventType() string {
	if s.Present {
		return EventHooksPresent
	}
	return EventHooksAbsent
}

// Payload is the event payload for session.
func (s HooksStatus) Payload(session string) map[string]interface{} {
	p := map[string]interface{}{"session": session, "role": s.Role, "path": s.Path}
	if !s.Present {
		p["reason"] = s.Reason
	}
	return p
}

// SyncSessionSettings is EnsureSettingsForRole for a starting session: it
// writes the managed settings for the role (what gt hooks sync writes), then
// checks the file the session will load. The error is EnsureSettingsForRole's;
// the status is absent whenever the guards are not in place, including when
// the file could not be written.
func SyncSessionSettings(settingsDir, workDir, role string) (HooksStatus, error) {
	return syncSessionSettings(hooks.EnvHome(), settingsDir, workDir, role)
}

func syncSessionSettings(home hooks.Home, settingsDir, workDir, role string) (HooksStatus, error) {
	err := ensureSettingsForRole(home, settingsDir, workDir, role)
	status := HooksStatus{Role: role, Path: filepath.Join(settingsDir, ".claude", "settings.json")}
	if err != nil {
		status.Reason = err.Error()
		return status, err
	}
	if checkErr := home.CheckManagedClaudeSettings(hooks.Target{
		Path: status.Path,
		Key:  hooks.ManagedTargetKey(role, settingsDir),
		Role: role,
	}); checkErr != nil {
		status.Reason = checkErr.Error()
		return status, nil
	}
	status.Present = true
	return status, nil
}

// ReportHooks records s for session as a feed event in townRoot's events log
// and, when the guards are absent, warns on stderr (the daemon log when the
// daemon starts the session).
func ReportHooks(townRoot, actor, session string, s HooksStatus) {
	_ = events.LogFeedTo(townRoot, s.EventType(), actor, s.Payload(session))
	if !s.Present {
		fmt.Fprintf(os.Stderr, "warning: %s %s (%s): %s\n", EventHooksAbsent, session, s.Role, s.Reason)
	}
}
