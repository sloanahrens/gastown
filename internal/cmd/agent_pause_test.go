package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/session"
)

// TestCheckPauseGatedOnlyAllowsPolecat pins the whitelist (gt-ahik, om
// kgx0): gt agent pause must refuse a target no scanner honors instead of
// silently writing a marker nothing reads. Only the polecat zombie/stall
// paths and the stuck-agent dog's polecat loop consult the marker.
func TestCheckPauseGatedOnlyAllowsPolecat(t *testing.T) {
	if err := checkPauseGated(session.RolePolecat); err != nil {
		t.Errorf("checkPauseGated(polecat) = %v, want nil", err)
	}

	for _, role := range []session.Role{
		session.RoleMayor,
		session.RoleDeacon,
		session.RoleWitness,
		session.RoleRefinery,
		session.RoleCrew,
	} {
		t.Run(string(role), func(t *testing.T) {
			err := checkPauseGated(role)
			if err == nil {
				t.Fatalf("checkPauseGated(%s) = nil, want a refusal (no scanner honors this role's marker)", role)
			}
			if !strings.Contains(err.Error(), string(role)) {
				t.Errorf("error %q does not name the refused role %q", err, role)
			}
		})
	}
}

// TestCheckPauseGatedDeaconHintsAtExistingCommand: deacon has its own,
// separate pause command that predates this one — the refusal should point
// there rather than leaving the operator to guess.
func TestCheckPauseGatedDeaconHintsAtExistingCommand(t *testing.T) {
	err := checkPauseGated(session.RoleDeacon)
	if err == nil {
		t.Fatal("checkPauseGated(deacon) = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "gt deacon pause") {
		t.Errorf("error %q does not point to `gt deacon pause`", err)
	}
}

// agentPauseAddresses are the forms an operator can hand to `gt agent
// pause`/`resume`: polecat as either the two-segment shorthand or the
// mail-style form, the rig singletons, crew, and the town-level agents.
var agentPauseAddresses = []string{
	"gastown/flint",
	"gastown/polecats/flint",
	"gastown/witness",
	"gastown/refinery",
	"gastown/crew/opal",
	"mayor",
	"mayor/",
	"deacon",
	"deacon/",
}

// TestPauseTargetCoordinatesMatchStatus pins the two address parsers together:
// the marker `gt agent pause <address>` writes must be the marker `gt status`
// reads to show that agent's pause. They parse independently, so a divergence
// means the operator freezes an agent and the status line never says so.
func TestPauseTargetCoordinatesMatchStatus(t *testing.T) {
	for _, address := range agentPauseAddresses {
		t.Run(address, func(t *testing.T) {
			target, err := parseAgentAddr(address)
			if err != nil {
				t.Fatalf("parseAgentAddr(%q): %v", address, err)
			}
			role, name := target.roleAndName()

			statusRig, statusRole, statusName, ok := agentMarkerTriple(address)
			if !ok {
				t.Fatalf("gt status does not recognize %q as a marker-backed agent", address)
			}
			if statusRig != target.Rig || statusRole != role || statusName != name {
				t.Errorf("pause target (%q, %q, %q) != status marker (%q, %q, %q) for %q",
					target.Rig, role, name, statusRig, statusRole, statusName, address)
			}
		})
	}
}

// TestPauseDisplayAddressIsCanonical pins the addressing fix from the om
// review on gt-wisp-6ajo: pause/resume output must name an agent the way the
// gt status banner does (one name per agent), and the name it prints must
// still parse back to the same agent so the "resume with" line is
// copy-pasteable.
func TestPauseDisplayAddressIsCanonical(t *testing.T) {
	const townRoot = "/town"
	for _, address := range agentPauseAddresses {
		t.Run(address, func(t *testing.T) {
			target, err := parseAgentAddr(address)
			if err != nil {
				t.Fatalf("parseAgentAddr(%q): %v", address, err)
			}
			role, name := target.roleAndName()
			display := target.displayAddress(role, name)

			// Same string the status banner derives from the marker path.
			markerPath := agentpause.FilePath(townRoot, target.Rig, role, name)
			if want := agentpause.AddressFromMarkerPath(markerPath); display != want {
				t.Errorf("display address = %q, but the status banner would say %q", display, want)
			}

			// Copy-pasteable: the printed address resolves back to the same
			// agent and the same marker coordinates.
			back, err := session.ParseAddress(display)
			if err != nil {
				t.Fatalf("printed address %q does not parse: %v", display, err)
			}
			if back.Rig != target.Rig || back.Name != name {
				t.Errorf("re-parsed %q = (%q, %q), want (%q, %q)", display, back.Rig, back.Name, target.Rig, name)
			}
			if addr := agentpause.AddressFor(back.Rig, string(back.Role), back.Name); addr != display {
				t.Errorf("re-parsed %q resolves to %q, want %q", display, addr, display)
			}
		})
	}
}
