package doctor

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// MalformedSessionNameCheck detects Gas Town tmux sessions whose names use the
// legacy naming scheme (e.g., "gt-whatsapp_automation-crew-max") rather than the
// current short-prefix format (e.g., "wa-crew-max").
//
// Detection uses explicit legacy-name matching rather than a parse round-trip.
// Round-trip detection cannot catch legacy names: "gt-whatsapp_automation-crew-max"
// parses as a polecat named "whatsapp_automation-crew-max" and round-trips to the
// same string — no mismatch is ever reported.
//
// Instead, we scan sessions for the pattern:
//
//	{any_prefix}-{registered_rig_name}-{role_suffix}
//
// where {registered_rig_name} is a known rig (e.g., "whatsapp_automation") and
// {role_suffix} is a crew role ("crew-{name}").
// The canonical name is then: {rig_short_prefix}-{role_suffix}.
//
// The check only reports: crew sessions may be attached, so they must be
// renamed manually.
type MalformedSessionNameCheck struct {
	BaseCheck
	sessionListerForTest SessionLister // Injectable for testing; nil uses real tmux
	registryForTest      *session.PrefixRegistry
}

type sessionRename struct {
	oldName string
	newName string
}

// NewMalformedSessionNameCheck creates a new malformed session name check.
func NewMalformedSessionNameCheck() *MalformedSessionNameCheck {
	return &MalformedSessionNameCheck{
		BaseCheck: BaseCheck{
			CheckName:        "session-name-format",
			CheckDescription: "Detect sessions with outdated Gas Town naming format",
			CheckCategory:    CategoryCleanup,
		},
	}
}

// Run detects sessions whose names use the legacy {prefix}-{rig_name}-{role} format.
func (c *MalformedSessionNameCheck) Run(ctx *CheckContext) *CheckResult {
	lister := c.sessionListerForTest
	if lister == nil {
		lister = &realSessionLister{t: tmux.NewTmux()}
	}

	reg := c.registryForTest
	if reg == nil {
		reg = session.DefaultRegistry()
	}

	sessions, err := lister.ListSessions()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not list tmux sessions",
			Details: []string{err.Error()},
		}
	}

	malformed := detectLegacySessionNames(sessions, reg)

	if len(malformed) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All Gas Town sessions use current naming format",
		}
	}

	var details []string
	for _, r := range malformed {
		details = append(details, fmt.Sprintf("Outdated: %s → should be %s (crew session — manual rename required)", r.oldName, r.newName))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d session(s) with outdated naming format", len(malformed)),
		Details: details,
		FixHint: "Crew sessions must be renamed manually: tmux rename-session -t OLD NEW",
	}
}

// detectLegacySessionNames scans sessions for the legacy
//
//	{any_prefix}-{registered_rig_name}-{role_suffix}
//
// pattern. For each match it computes the canonical name
//
//	{rig_short_prefix}-{role_suffix}
//
// and returns the rename list.
func detectLegacySessionNames(sessions []string, reg *session.PrefixRegistry) []sessionRename {
	rigs := reg.AllRigs() // rigName → shortPrefix
	if len(rigs) == 0 {
		return nil
	}

	// Build set of known Gastown prefixes for ownership gating.
	knownPrefixes := make(map[string]bool)
	for _, prefix := range rigs {
		knownPrefixes[prefix] = true
	}

	var result []sessionRename
	seen := make(map[string]bool)

	for _, sess := range sessions {
		if sess == "" || seen[sess] {
			continue
		}
		r, ok := matchLegacyName(sess, rigs, knownPrefixes)
		if ok {
			seen[sess] = true
			result = append(result, r)
		}
	}
	return result
}

// matchLegacyName checks whether sess matches the old
//
//	{known_prefix}-{rig_name}-crew-{name}
//
// pattern for any known rig, and returns the canonical rename if so.
// The prefix before the rig name must be a known Gastown prefix to avoid
// false-positives on non-Gastown sessions (e.g., "my-niflheim-crew-max")
// and polecat sessions whose names embed rig names (e.g., "gt-fix-gastown-crew-max").
func matchLegacyName(sess string, rigs map[string]string, knownPrefixes map[string]bool) (sessionRename, bool) {
	for rigName, shortPrefix := range rigs {
		// Look for "-{rigName}-" anywhere in the session name.
		needle := "-" + rigName + "-"
		idx := strings.Index(sess, needle)
		if idx < 0 {
			continue
		}

		// Ownership guard: the part before the rig name must be a known
		// Gastown prefix. This prevents matching non-Gastown sessions
		// and polecat sessions whose names happen to contain a rig name.
		sessionPrefix := sess[:idx]
		if !knownPrefixes[sessionPrefix] {
			continue
		}

		// Skip if the session already uses the correct prefix for this rig.
		if sessionPrefix == shortPrefix {
			continue
		}

		// The part after the rig name is the role suffix.
		roleSuffix := sess[idx+len(needle):]
		if roleSuffix == "" {
			continue
		}

		// Validate: must be a known Gas Town role suffix.
		if !isValidRoleSuffix(roleSuffix) {
			continue
		}

		return sessionRename{
			oldName: sess,
			newName: shortPrefix + "-" + roleSuffix,
		}, true
	}
	return sessionRename{}, false
}

// isValidRoleSuffix returns true if suffix is a crew role identifier ("crew-{name}").
func isValidRoleSuffix(suffix string) bool {
	return strings.HasPrefix(suffix, "crew-") && len(suffix) > len("crew-")
}
