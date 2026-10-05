// Package role owns the agent role vocabulary: the role names Gas Town uses
// in GT_ROLE and on session names, the parser that turns a GT_ROLE string
// into (role, rig, worker), and the beads actor string derived from them.
//
// It is a leaf so that packages which cannot import internal/cmd — the
// command-logic packages the thin-cobra order (D10) is pulling out of it —
// can name a role without re-deriving the vocabulary. internal/cmd keeps the
// Role type as an alias and delegates its own parser here.
package role

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// Role is one agent role.
type Role string

// The roles Gas Town names, sourced from internal/constants so there is one
// place the names are spelled. Unknown covers both "no role detected" and a
// GT_ROLE that names something deleted (the boot, witness and refinery roles
// are gone).
const (
	Polecat Role = constants.RolePolecat
	Crew    Role = constants.RoleCrew
	Unknown Role = "unknown"
)

// All returns every Role Parse can produce. It is the enumeration half of the
// single source of truth for actor construction: combined with Actor, it lets
// a caller verify every actor string can be accounted for, instead of a
// hand-maintained list kept in sync by hand (gt-9pn).
func All() []Role {
	return []Role{Polecat, Crew, Unknown}
}

// Parse reads a GT_ROLE-style string and returns the role, the rig and the
// worker name, the last two empty when the string names neither.
//
// Accepted shapes:
//
//	"polecat"                   -> Polecat, "", ""   (rig and name come from GT_RIG/GT_POLECAT)
//	"gastown/polecats/alpha"    -> Polecat, "gastown", "alpha"
//	"gastown/crew/max"          -> Crew, "gastown", "max"
func Parse(s string) (Role, string, string) {
	s = strings.TrimSpace(s)

	// Normalize consecutive slashes (e.g. "gamestore//refinery" → "gamestore/refinery")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")

	// Compound roles: rig/role or rig/polecats/name or rig/crew/name
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		// Unknown format, try to match as simple role
		return Role(s), "", ""
	}

	rig := parts[0]

	// daemon/<job> (daemon/plugin for script plugins, daemon/spec-dispatch,
	// daemon/patrol-scan, ...) is the daemon itself, never a polecat. Read
	// through the rig/polecatName fallback below it was polecat "plugin" of
	// rig "daemon", and gt sling refused every seat-refill dispatch (gt-vsc9w).
	if rig == "daemon" {
		return Role(s), "", ""
	}

	switch parts[1] {
	case "boot", "witness", "refinery":
		// The boot, witness (gt-4k3fj.6.1) and refinery (gt-v4ssj.6) roles
		// were deleted. A stale GT_ROLE of deacon/boot, <rig>/witness or
		// <rig>/refinery is an unknown role, not a polecat of that name (a
		// name the pool already reserves).
		return Role(s), "", ""
	case "polecats":
		if len(parts) >= 3 {
			return Polecat, rig, parts[2]
		}
		return Polecat, rig, ""
	case constants.RoleCrew:
		if len(parts) >= 3 {
			return Crew, rig, parts[2]
		}
		return Crew, rig, ""
	default:
		// Might be rig/polecatName format
		return Polecat, rig, parts[1]
	}
}

// Actor returns the actor identity string for beads attribution, matching the
// beads created_by convention:
//
//   - Bare roles with no rig or worker name: "polecat", "crew"
//   - Workers: "gastown/crew/max", "gastown/polecats/Toast"
func Actor(r Role, rig, name string) string {
	switch r {
	case Polecat:
		if rig != "" && name != "" {
			return fmt.Sprintf("%s/polecats/%s", rig, name)
		}
		return "polecat"
	case Crew:
		if rig != "" && name != "" {
			return fmt.Sprintf("%s/crew/%s", rig, name)
		}
		return "crew"
	default:
		return string(r)
	}
}
