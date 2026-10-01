package deliver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/townlog"
)

// Town delivers nudges by target address inside one town, as `gt nudge
// <target> <message>` does: it resolves the target to a session, skips a
// target in DND, delivers through Delivery and logs the nudge.
//
// Channel targets (channel:<name>) are not supported here; they stay with
// gt nudge, which reads the messaging config and lists the town's agents.
type Town struct {
	// Delivery delivers to the resolved session; its TownRoot is the town's
	// ("" when the caller is outside any town).
	Delivery *Delivery
	// Registry maps rigs to session prefixes; nil is session.DefaultRegistry().
	Registry *session.PrefixRegistry
	// NotificationLevel reads the notification level of an agent bead (DND);
	// nil reads it with bd from the town root.
	NotificationLevel func(townRoot, agentBeadID string) (string, error)
	// Log records a delivered nudge in town.log and the event feed; nil
	// writes both under the town root.
	Log func(townRoot, sender, rig, target, message string)
}

func (n *Town) registry() *session.PrefixRegistry {
	if n.Registry != nil {
		return n.Registry
	}
	return session.DefaultRegistry()
}

func (n *Town) notificationLevel(townRoot, agentBeadID string) (string, error) {
	if n.NotificationLevel != nil {
		return n.NotificationLevel(townRoot, agentBeadID)
	}
	return beads.New(townRoot).GetAgentNotificationLevel(agentBeadID)
}

func (n *Town) log(townRoot, sender, rig, target, message string) {
	if n.Log != nil {
		n.Log(townRoot, sender, rig, target, message)
		return
	}
	_ = townlog.NewLogger(townRoot).Log(townlog.EventNudge, target, strings.TrimSpace(message))
	_ = events.LogFeedTo(townRoot, events.TypeNudge, sender, events.NudgePayload(rig, target, message))
}

// Nudge delivers message from sender to target: a role shortcut ("mayor"), a
// rig address ("gastown/max", "gastown/crew/max", "gastown/polecats/toast")
// or a raw session name.
func (n *Town) Nudge(ctx context.Context, target, message, sender string) error {
	d := n.Delivery
	townRoot := d.TownRoot
	reg := n.registry()

	// Normalize trailing slash: the mail system uses "mayor/" as the
	// canonical address, but nudge role shortcuts expect bare names.
	target = strings.TrimSuffix(target, "/")
	if strings.HasPrefix(target, "channel:") {
		return fmt.Errorf("channel target %q: channel nudges go through gt nudge", target)
	}

	// Check DND status for target (unless forced).
	if townRoot != "" && !d.Force {
		if beadID := AgentBeadID(reg, target); beadID != "" {
			// An agent bead that cannot be read allows the nudge (fail-open
			// for backward compatibility).
			if level, err := n.notificationLevel(townRoot, beadID); err == nil && level == beads.NotifyMuted {
				fmt.Fprintf(d.Stderr, "Target has DND enabled (%s) - nudge skipped\n", level)
				return nil
			}
		}
	}

	// Expand role shortcuts to session names.
	if target == constants.RoleMayor {
		target = session.MayorSessionName()
	}
	if strings.HasPrefix(target, constants.RoleMayor+"/") || strings.HasPrefix(target, "deacon/") {
		return fmt.Errorf("invalid town target %q", target)
	}

	if !strings.Contains(target, "/") {
		// Raw session name.
		exists, err := d.Tmux.HasSession(target)
		if err != nil {
			return fmt.Errorf("checking session: %w", err)
		}
		if !exists {
			return fmt.Errorf("session %q not found", target)
		}
		if err := d.Deliver(ctx, target, message, sender); err != nil {
			return fmt.Errorf("nudging session: %w", err)
		}
		if townRoot != "" {
			n.log(townRoot, sender, "", target, message)
		}
		return nil
	}

	rigName, name, ok := strings.Cut(target, "/")
	if !ok || rigName == "" || name == "" {
		return fmt.Errorf("invalid address format: expected 'rig/polecat', got '%s'", target)
	}
	prefix := reg.PrefixForRig(rigName)
	var sessionName string
	switch {
	case strings.HasPrefix(name, "crew/"):
		sessionName = session.CrewSessionName(prefix, strings.TrimPrefix(name, "crew/"))
	case strings.HasPrefix(name, "polecats/"):
		// Explicit polecat address: bypasses crew-first resolution.
		sessionName = session.PolecatSessionName(prefix, strings.TrimPrefix(name, "polecats/"))
	default:
		// Short address: could be crew or polecat. Try crew first (matches
		// the mail system's addressToSessionIDs), then fall back to polecat.
		sessionName = session.CrewSessionName(prefix, name)
		if exists, _ := d.Tmux.HasSession(sessionName); !exists {
			sessionName = session.PolecatSessionName(prefix, name)
		}
	}

	// For queue/wait-idle modes, verify the session exists before enqueuing:
	// a queue file for a nonexistent session is written but never drained.
	if d.Mode != ModeImmediate {
		exists, err := d.Tmux.HasSession(sessionName)
		if err != nil {
			return fmt.Errorf("checking session: %w", err)
		}
		if !exists {
			return fmt.Errorf("session %q not found (cannot queue nudge for nonexistent session)", sessionName)
		}
	}
	if err := d.Deliver(ctx, sessionName, message, sender); err != nil {
		return fmt.Errorf("nudging session: %w", err)
	}
	if townRoot != "" {
		n.log(townRoot, sender, rigName, target, message)
	}
	return nil
}

// AgentBeadID converts a nudge target address to the agent bead whose
// notification level gates it ("" when the address names none).
// Examples:
//   - "mayor" -> "hq-mayor"
//   - "gastown/alpha" -> "gt-alpha"
func AgentBeadID(reg *session.PrefixRegistry, address string) string {
	switch address {
	case constants.RoleMayor, constants.RoleMayor + "/":
		return session.MayorSessionName()
	}
	if strings.HasPrefix(address, constants.RoleMayor+"/") || strings.HasPrefix(address, "deacon/") {
		return ""
	}

	rig, role, ok := strings.Cut(address, "/")
	if !ok {
		return ""
	}
	if strings.HasPrefix(role, "crew/") {
		return session.CrewSessionName(reg.PrefixForRig(rig), strings.TrimPrefix(role, "crew/"))
	}
	if strings.HasPrefix(role, "polecats/") {
		return session.PolecatSessionName(reg.PrefixForRig(rig), strings.TrimPrefix(role, "polecats/"))
	}
	// Assume polecat
	return session.PolecatSessionName(reg.PrefixForRig(rig), role)
}

// Sender is who `gt nudge` attributes a nudge to when it runs in cwd, inside
// townRoot ("" when cwd is in no town), with the identity environment getenv
// reads: GT_ROLE (completed by GT_RIG, GT_CREW, GT_POLECAT and then the
// directory) or, without it, the role the directory is the home of.
// "unknown" when neither names one, as for the daemon at the town root.
func Sender(cwd, townRoot string, getenv func(string) string) string {
	if townRoot == "" {
		return "unknown"
	}
	role, rig, name := roleFromDir(cwd, townRoot)
	if envRole := getenv("GT_ROLE"); envRole != "" {
		dirRig, dirName := rig, name
		role, rig, name = parseRole(envRole)
		if rig == "" {
			rig = getenv("GT_RIG")
		}
		if name == "" {
			if crew := getenv("GT_CREW"); crew != "" {
				name = crew
			} else {
				name = getenv("GT_POLECAT")
			}
		}
		if role == roleCrew || role == rolePolecat {
			if rig == "" {
				rig = dirRig
			}
			if name == "" {
				name = dirName
			}
		}
	}
	switch role {
	case constants.RoleMayor:
		return constants.RoleMayor
	case roleCrew:
		return fmt.Sprintf("%s/crew/%s", rig, name)
	case rolePolecat:
		return fmt.Sprintf("%s/%s", rig, name)
	default:
		return role
	}
}

const (
	roleCrew    = "crew"
	rolePolecat = "polecat"
	roleUnknown = "unknown"
)

// roleFromDir is the role whose home dir is (the mayor, a crew member or a
// polecat), and the rig and name the path names even when it is no role's
// home.
func roleFromDir(dir, townRoot string) (role, rig, name string) {
	rel, err := filepath.Rel(townRoot, dir)
	if err != nil {
		return roleUnknown, "", ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" {
		// The town root is a neutral location.
		return roleUnknown, "", ""
	}
	parts := strings.Split(rel, "/")
	switch parts[0] {
	case constants.RoleMayor:
		return constants.RoleMayor, "", ""
	case "deacon":
		// deacon/ held the deleted deacon, boot and dog roles; it is not a rig.
		return roleUnknown, "", ""
	}
	rig = parts[0]
	if len(parts) >= 2 {
		switch parts[1] {
		case constants.RoleMayor:
			return constants.RoleMayor, rig, ""
		case "polecats":
			if len(parts) >= 3 {
				return rolePolecat, rig, parts[2]
			}
		case "crew":
			if len(parts) >= 3 {
				return roleCrew, rig, parts[2]
			}
		}
	}
	return roleUnknown, rig, ""
}

// parseRole parses a GT_ROLE value like "mayor", "gastown/crew/max" or
// "gastown/polecats/alpha".
func parseRole(s string) (role, rig, name string) {
	s = strings.TrimSpace(s)
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")
	if s == constants.RoleMayor {
		return constants.RoleMayor, "", ""
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return s, "", ""
	}
	rig = parts[0]
	switch parts[1] {
	case "boot", "witness", "refinery":
		// Deleted roles: a stale GT_ROLE naming one is an unknown role, not a
		// polecat of that name.
		return s, "", ""
	case "polecats":
		if len(parts) >= 3 {
			return rolePolecat, rig, parts[2]
		}
		return rolePolecat, rig, ""
	case roleCrew:
		if len(parts) >= 3 {
			return roleCrew, rig, parts[2]
		}
		return roleCrew, rig, ""
	default:
		return rolePolecat, rig, parts[1]
	}
}
