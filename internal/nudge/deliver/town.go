package deliver

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/townlog"
)

// Town delivers nudges by target address inside one town: it is `gt nudge
// <target> <message>`. It resolves the target to a session (or a channel to
// its members), skips a target in DND, delivers through Delivery and logs the
// nudge.
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
	// RigExists reports whether rig is one of the town's rigs; nil reads
	// mayor/rigs.json.
	RigExists func(townRoot, rig string) bool
	// Channels reads the town's nudge channels (name to member patterns);
	// nil reads config/messaging.json.
	Channels func(townRoot string) (map[string][]string, error)
	// Skipped reports a target DND skipped; nil prints a line to
	// Delivery.Stderr.
	Skipped func(target, level string)
	// Member reports each channel member's outcome as it is done; nil
	// reports nothing.
	Member func(ChannelMember)
}

// ChannelMember is what a channel nudge did for one member session.
type ChannelMember struct {
	Session string
	DND     string // the notification level that skipped it; "" when it was not skipped
	Err     error  // the delivery's error
}

// channelGap is the pause between two channel members' deliveries.
const channelGap = 100 * time.Millisecond

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

func (n *Town) rigExists(townRoot, rig string) bool {
	if n.RigExists != nil {
		return n.RigExists(townRoot, rig)
	}
	rigs, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return false
	}
	_, ok := rigs.Rigs[rig]
	return ok
}

func (n *Town) channels(townRoot string) (map[string][]string, error) {
	if n.Channels != nil {
		return n.Channels(townRoot)
	}
	cfg, err := config.LoadMessagingConfig(config.MessagingConfigPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading messaging config: %w", err)
	}
	return cfg.NudgeChannels, nil
}

func (n *Town) skipped(target, level string) {
	if n.Skipped != nil {
		n.Skipped(target, level)
		return
	}
	fmt.Fprintf(n.Delivery.Stderr, "Target has DND enabled (%s) - nudge skipped\n", level)
}

// muted is the notification level that skips a nudge to address, or "" when
// none does: forced, outside a town, no agent bead, or one that cannot be
// read (fail-open for backward compatibility).
func (n *Town) muted(address string) string {
	d := n.Delivery
	if d.TownRoot == "" || d.Force || address == "" {
		return ""
	}
	beadID := AgentBeadID(n.registry(), address)
	if beadID == "" {
		return ""
	}
	if level, err := n.notificationLevel(d.TownRoot, beadID); err == nil && level == beads.NotifyMuted {
		return level
	}
	return ""
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
// rig address ("gastown/max", "gastown/crew/max", "gastown/polecats/toast"),
// a raw session name or a channel ("channel:<name>", NudgeChannel).
func (n *Town) Nudge(ctx context.Context, target, message, sender string) error {
	d := n.Delivery
	townRoot := d.TownRoot
	reg := n.registry()

	// Normalize trailing slash: the mail system uses "mayor/" as the
	// canonical address, but nudge role shortcuts expect bare names.
	target = strings.TrimSuffix(target, "/")
	if channel, ok := strings.CutPrefix(target, "channel:"); ok {
		_, err := n.NudgeChannel(ctx, channel, message, sender)
		return err
	}

	if level := n.muted(target); level != "" {
		n.skipped(target, level)
		return nil
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
	// A polecat's session is named by its rig's prefix, which an unknown rig
	// does not have: refuse it rather than nudge another rig's session.
	polecat := func(name string) (string, error) {
		if townRoot == "" {
			return "", fmt.Errorf("not in a Gas Town workspace")
		}
		if !n.rigExists(townRoot, rigName) {
			return "", fmt.Errorf("rig '%s' not found", rigName)
		}
		return session.PolecatSessionName(prefix, name), nil
	}
	var sessionName string
	switch {
	case strings.HasPrefix(name, "crew/"):
		sessionName = session.CrewSessionName(prefix, strings.TrimPrefix(name, "crew/"))
	case strings.HasPrefix(name, "polecats/"):
		// Explicit polecat address: bypasses crew-first resolution.
		var err error
		if sessionName, err = polecat(strings.TrimPrefix(name, "polecats/")); err != nil {
			return err
		}
	default:
		// Short address: could be crew or polecat. Try crew first (matches
		// the mail system's addressToSessionIDs), then fall back to polecat.
		sessionName = session.CrewSessionName(prefix, name)
		if exists, _ := d.Tmux.HasSession(sessionName); !exists {
			var err error
			if sessionName, err = polecat(name); err != nil {
				return err
			}
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

// NudgeChannel delivers message from sender to every running member of the
// town's nudge channel, reporting each through Member, skipping members in
// DND, and logs the nudge once. It returns the members' outcomes, and an
// error when the channel cannot be resolved or a delivery failed. A channel
// with no running member is no error: it returns no outcomes.
func (n *Town) NudgeChannel(ctx context.Context, channel, message, sender string) ([]ChannelMember, error) {
	d := n.Delivery
	if d.TownRoot == "" {
		return nil, fmt.Errorf("channel nudge: not in a Gas Town workspace")
	}
	channels, err := n.channels(d.TownRoot)
	if err != nil {
		return nil, err
	}
	patterns, ok := channels[channel]
	if !ok {
		return nil, fmt.Errorf("nudge channel %q not found in messaging config", channel)
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("nudge channel %q has no members", channel)
	}
	live, err := d.Tmux.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	members := ChannelSessions(n.registry(), patterns, live)
	if len(members) == 0 {
		return nil, nil
	}

	var results []ChannelMember
	failed := 0
	for i, sessionName := range members {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		if i > 0 {
			d.Clock.Sleep(channelGap)
		}
		r := ChannelMember{Session: sessionName}
		if r.DND = n.muted(SessionAddress(n.registry(), sessionName)); r.DND == "" {
			if r.Err = d.Deliver(ctx, sessionName, message, sender); r.Err != nil {
				failed++
			}
		}
		results = append(results, r)
		if n.Member != nil {
			n.Member(r)
		}
	}
	n.log(d.TownRoot, sender, "", "channel:"+channel, message)
	if failed > 0 {
		return results, fmt.Errorf("%d nudge(s) failed", failed)
	}
	return results, nil
}

// ChannelSessions resolves a nudge channel's member patterns to the running
// agent sessions among live, each once, in pattern order. A pattern is a
// role ("mayor"), a rig address ("gastown/crew/max", "gastown/polecats/toast",
// or "gastown/toast" for a polecat) or one with "*" for the rig or the name
// ("gastown/polecats/*", "*/crew/*").
func ChannelSessions(reg *session.PrefixRegistry, patterns, live []string) []string {
	var agents []*session.AgentIdentity
	names := map[*session.AgentIdentity]string{}
	for _, name := range live {
		id, err := session.ParseSessionNameWithRegistry(name, reg)
		if err != nil {
			continue
		}
		switch id.Role {
		case session.RoleMayor, session.RoleCrew, session.RolePolecat:
			agents = append(agents, id)
			names[id] = name
		}
	}
	// The mayor first, then by rig, crew before polecats, then by name.
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i], agents[j]
		if (a.Role == session.RoleMayor) != (b.Role == session.RoleMayor) {
			return a.Role == session.RoleMayor
		}
		if a.Rig != b.Rig {
			return a.Rig < b.Rig
		}
		if a.Role != b.Role {
			return a.Role == session.RoleCrew
		}
		return a.Name < b.Name
	})

	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, pattern := range patterns {
		if pattern == constants.RoleMayor {
			add(session.MayorSessionName())
			continue
		}
		rigPattern, target, ok := strings.Cut(pattern, "/")
		if !ok {
			continue
		}
		role, name := session.RolePolecat, target
		if rest, ok := strings.CutPrefix(target, "polecats/"); ok {
			name = rest
		} else if rest, ok := strings.CutPrefix(target, "crew/"); ok {
			role, name = session.RoleCrew, rest
		}
		for _, a := range agents {
			if a.Role != role || (rigPattern != "*" && rigPattern != a.Rig) {
				continue
			}
			// A bare legacy polecat name ("gastown/toast") takes no wildcard.
			if name != a.Name && (name != "*" || target == "*") {
				continue
			}
			add(names[a])
		}
	}
	return out
}

// SessionAddress is the nudge address of an agent session ("" when the name
// is no agent's): "hq-mayor" -> "mayor", "gt-crew-max" -> "gastown/crew/max",
// "gt-alpha" -> "gastown/alpha".
func SessionAddress(reg *session.PrefixRegistry, sessionName string) string {
	id, err := session.ParseSessionNameWithRegistry(sessionName, reg)
	if err != nil {
		return ""
	}
	switch id.Role {
	case session.RoleMayor:
		return constants.RoleMayor
	case session.RoleCrew:
		return fmt.Sprintf("%s/crew/%s", id.Rig, id.Name)
	case session.RolePolecat:
		return fmt.Sprintf("%s/%s", id.Rig, id.Name)
	default:
		return ""
	}
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
