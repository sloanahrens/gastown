package doctor

import (
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// SessionEnvReader abstracts tmux session environment reads for testing.
type SessionEnvReader interface {
	ListSessions() ([]string, error)
	GetAllEnvironment(session string) (map[string]string, error)
}

// SessionEnvWriter abstracts tmux session environment writes for testing.
type SessionEnvWriter interface {
	SetEnvironment(session, key, value string) error
}

// SessionEnvAccessor combines read and write access to tmux session environments.
type SessionEnvAccessor interface {
	SessionEnvReader
	SessionEnvWriter
}

// tmuxEnvReaderWriter wraps real tmux operations for both reading and writing.
type tmuxEnvReaderWriter struct {
	t *tmux.Tmux
}

func (r *tmuxEnvReaderWriter) ListSessions() ([]string, error) {
	return r.t.ListSessions()
}

func (r *tmuxEnvReaderWriter) GetAllEnvironment(session string) (map[string]string, error) {
	return r.t.GetAllEnvironment(session)
}

func (r *tmuxEnvReaderWriter) SetEnvironment(session, key, value string) error {
	return r.t.SetEnvironment(session, key, value)
}

// envCheckedRoles are the live session roles whose environment the check
// verifies. Sessions of any other parsed role are skipped.
var envCheckedRoles = map[session.Role]bool{
	session.RoleOverseer: true,
	session.RoleCrew:     true,
	session.RolePolecat:  true,
}

// sessionGetenv resolves a session's expected agent env against the session
// itself for GOFLAGS, so the verdict does not depend on the environment of the
// process running the doctor (gt-jb4zh). Every other variable still reads
// processEnv.
func sessionGetenv(actual map[string]string, processEnv func(string) string) func(string) string {
	return func(key string) string {
		if key == "GOFLAGS" {
			return actual["GOFLAGS"]
		}
		return processEnv(key)
	}
}

// EnvVarsCheck verifies that tmux session environment variables match expected values.
type EnvVarsCheck struct {
	FixableCheck
	reader   SessionEnvReader    // nil means use real tmux
	accessor SessionEnvAccessor  // non-nil when Fix() support is needed
	getenv   func(string) string // the doctor process's environment; nil means os.Getenv

	newTmuxForTest func() *tmux.Tmux // nil means the real town-socket client
}

// hostEnvTmux is the real client for the check: Routed, so it neither routes
// nor lists the sandbox sessions. Fix writes host-derived values (TownRoot and
// the like) into a session's environment, and a sandbox session's environment
// holds the sandbox's values.
func (c *EnvVarsCheck) hostEnvTmux() *tmuxEnvReaderWriter {
	build := tmux.NewTmux
	if c.newTmuxForTest != nil {
		build = c.newTmuxForTest
	}
	return &tmuxEnvReaderWriter{t: build().Routed()}
}

// processEnv is the doctor process's environment, read for the variables that
// do not come from the session being checked.
func (c *EnvVarsCheck) processEnv() func(string) string {
	if c.getenv != nil {
		return c.getenv
	}
	return os.Getenv
}

// NewEnvVarsCheck creates a new env vars check.
func NewEnvVarsCheck() *EnvVarsCheck {
	return &EnvVarsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "env-vars",
				CheckDescription: "Verify tmux session environment variables match expected values",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// NewEnvVarsCheckWithReader creates a check with a custom reader (for testing Run()).
func NewEnvVarsCheckWithReader(reader SessionEnvReader) *EnvVarsCheck {
	c := NewEnvVarsCheck()
	c.reader = reader
	return c
}

// NewEnvVarsCheckWithAccessor creates a check with a custom accessor (for testing Fix()).
func NewEnvVarsCheckWithAccessor(accessor SessionEnvAccessor) *EnvVarsCheck {
	c := NewEnvVarsCheck()
	c.accessor = accessor
	c.reader = accessor
	return c
}

// Run checks environment variables for all Gas Town sessions.
func (c *EnvVarsCheck) Run(ctx *CheckContext) *CheckResult {
	reader := c.reader
	if reader == nil {
		reader = c.hostEnvTmux()
	}

	sessions, err := reader.ListSessions()
	if err != nil {
		// Could not enumerate tmux sessions at all — this could mean no
		// server is running, or it could mean something else went wrong.
		// Either way we didn't verify anything, so it's not a pass.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not list tmux sessions",
			Details: []string{err.Error()},
		}
	}

	// Filter to Gas Town sessions only (known rig prefixes and hq-*)
	var gtSessions []string
	for _, sess := range sessions {
		if ctx.prefixes().IsKnownSession(sess) {
			gtSessions = append(gtSessions, sess)
		}
	}

	if len(gtSessions) == 0 {
		// No Gas Town sessions - treat as success (valid when Gas Town is down)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No Gas Town sessions running",
		}
	}

	var mismatches []string
	var beadsDirWarnings []string
	checkedCount := 0

	for _, sess := range gtSessions {
		identity, err := session.ParseSessionNameWithRegistry(sess, ctx.prefixes())
		if err != nil {
			// Skip unparseable sessions
			continue
		}
		if !envCheckedRoles[identity.Role] {
			// Leftover session of a retired role: nothing sets its env any more.
			continue
		}

		// Get actual tmux env vars
		actual, err := reader.GetAllEnvironment(sess)
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("%s: could not read env vars: %v", sess, err))
			continue
		}

		checkedCount++

		// Get expected env vars based on role, with GOFLAGS taken from this session
		expected := config.AgentEnv(config.AgentEnvConfig{
			Role:      string(identity.Role),
			Rig:       identity.Rig,
			AgentName: identity.Name,
			TownRoot:  ctx.TownRoot,
			Getenv:    sessionGetenv(actual, c.processEnv()),
		})

		// Compare each expected var
		for key, expectedVal := range expected {
			actualVal, exists := actual[key]
			if !exists && expectedVal != "" {
				// Only flag missing vars when the expected value is non-empty.
				// An absent var has the same effect as an empty one (e.g. CLAUDECODE=""
				// prevents nested session detection, but so does CLAUDECODE being unset).
				mismatches = append(mismatches, fmt.Sprintf("%s: missing %s (expected %q)", sess, key, expectedVal))
			} else if exists && actualVal != expectedVal {
				mismatches = append(mismatches, fmt.Sprintf("%s: %s=%q (expected %q)", sess, key, actualVal, expectedVal))
			}
		}

		// Check for BEADS_DIR - this breaks routing-based lookups
		if beadsDir, exists := actual["BEADS_DIR"]; exists && beadsDir != "" {
			beadsDirWarnings = append(beadsDirWarnings, fmt.Sprintf("%s: BEADS_DIR=%q (breaks prefix routing)", sess, beadsDir))
		}
	}

	// Check for BEADS_DIR issues first (higher priority warning)
	if len(beadsDirWarnings) > 0 {
		details := beadsDirWarnings
		if len(mismatches) > 0 {
			details = append(details, "", "Other env var issues:")
			details = append(details, mismatches...)
		}
		details = append(details,
			"",
			"BEADS_DIR overrides prefix-based routing and breaks multi-rig lookups.",
		)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Found BEADS_DIR set in %d session(s)", len(beadsDirWarnings)),
			Details: details,
			FixHint: "Remove BEADS_DIR from session environment: gt shutdown && gt up",
		}
	}

	if len(mismatches) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d session(s) have correct environment variables", checkedCount),
		}
	}

	// Add explanation about needing restart
	details := append(mismatches,
		"",
		"Note: Mismatched session env vars won't affect running Claude until sessions restart.",
	)

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d env var mismatch(es) across %d session(s)", len(mismatches), checkedCount),
		Details: details,
		FixHint: "Run 'gt doctor fix env-vars' to apply missing env vars in-place, or 'gt shutdown && gt up' to restart",
	}
}

// Fix applies missing or incorrect env vars to all Gas Town tmux sessions in-place.
// The running Claude process is unaffected (it already has env vars from startup);
// this updates the tmux session store so future processes and gt doctor agree.
func (c *EnvVarsCheck) Fix(ctx *CheckContext) error {
	accessor := c.accessor
	if accessor == nil {
		accessor = c.hostEnvTmux()
	}

	sessions, err := accessor.ListSessions()
	if err != nil {
		// No tmux server — nothing to fix.
		return nil
	}

	for _, sess := range sessions {
		if !ctx.prefixes().IsKnownSession(sess) {
			continue
		}
		identity, err := session.ParseSessionNameWithRegistry(sess, ctx.prefixes())
		if err != nil || !envCheckedRoles[identity.Role] {
			continue
		}

		actual, err := accessor.GetAllEnvironment(sess)
		if err != nil {
			continue
		}

		expected := config.AgentEnv(config.AgentEnvConfig{
			Role:      string(identity.Role),
			Rig:       identity.Rig,
			AgentName: identity.Name,
			TownRoot:  ctx.TownRoot,
			Getenv:    sessionGetenv(actual, c.processEnv()),
		})

		for key, expectedVal := range expected {
			actualVal, exists := actual[key]
			if !exists || actualVal != expectedVal {
				_ = accessor.SetEnvironment(sess, key, expectedVal)
			}
		}
	}
	return nil
}
