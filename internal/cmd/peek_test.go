package cmd

import (
	"strings"
	"testing"
)

// TestPeekRetiredBootRoleIsNotASession pins gt-xxvo9: the boot role was
// deleted (gt-4k3fj.6.1), so neither "boot" nor "hq/boot" may resolve to the
// hq-boot pane. The town-agent table is the only place gt peek can produce
// that session name, so an entry there is the regression this test catches.
func TestPeekRetiredBootRoleIsNotASession(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"boot", "hq/boot"} {
		if sessionName, ok := townAgentSessions[address]; ok {
			t.Errorf("gt peek %s maps to session %q; the boot role is retired", address, sessionName)
		}
	}

	// Fall-through, not a capture: "hq/boot" is now just a rig address, and
	// the bare "boot" has no rig to resolve against, so both fail in address
	// resolution rather than reaching a pane.
	rig, name, err := parseAddress("hq/boot")
	if err != nil {
		t.Fatalf("parseAddress(hq/boot) = %v, want the rig/polecat split", err)
	}
	if rig != "hq" || name != "boot" {
		t.Errorf("parseAddress(hq/boot) = (%q, %q), want (hq, boot)", rig, name)
	}
	for _, address := range []string{"boot", "hq/boot"} {
		err := runPeek(peekCmd, []string{address})
		if err == nil {
			t.Fatalf("runPeek(%q) = nil, want an error", address)
		}
		if strings.Contains(err.Error(), "capturing") {
			t.Errorf("runPeek(%q) reached pane capture: %v", address, err)
		}
	}
}
