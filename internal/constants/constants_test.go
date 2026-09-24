package constants

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRoleEmoji(t *testing.T) {
	tests := []struct {
		role   string
		expect string
	}{
		{RoleMayor, EmojiMayor},
		{RoleDeacon, EmojiDeacon},
		{RoleWitness, EmojiWitness},
		{RoleRefinery, EmojiRefinery},
		{RoleCrew, EmojiCrew},
		{RolePolecat, EmojiPolecat},
		{"unknown", "❓"},
		{"", "❓"},
	}
	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			got := RoleEmoji(tt.role)
			if got != tt.expect {
				t.Errorf("RoleEmoji(%q) = %q, want %q", tt.role, got, tt.expect)
			}
		})
	}
}

func TestBeadsCustomTypesList(t *testing.T) {
	types := BeadsCustomTypesList()
	expected := []string{"agent", "role", "rig", "convoy", "slot", "queue", "event", "message", "molecule", "gate", "merge-request"}

	if len(types) != len(expected) {
		t.Fatalf("BeadsCustomTypesList() returned %d items, want %d", len(types), len(expected))
	}
	for i, typ := range types {
		if typ != expected[i] {
			t.Errorf("BeadsCustomTypesList()[%d] = %q, want %q", i, typ, expected[i])
		}
	}
}

func TestBeadsInfraTypesList(t *testing.T) {
	types := BeadsInfraTypesList()
	expected := []string{"agent", "role", "message"}

	if len(types) != len(expected) {
		t.Fatalf("BeadsInfraTypesList() returned %d items, want %d", len(types), len(expected))
	}
	for i, typ := range types {
		if typ != expected[i] {
			t.Errorf("BeadsInfraTypesList()[%d] = %q, want %q", i, typ, expected[i])
		}
		if typ == "rig" {
			t.Fatal("rig must stay durable and must not be an infra type")
		}
	}
}

func TestNonDispatchableBeadWispTypes(t *testing.T) {
	got := NonDispatchableBeadWispTypes()
	// Every entry must be one of the durable types, "wisp" excluded (the wisp
	// scan walks the wisps table, where every row is a wisp), and order kept.
	for i, typ := range got {
		found := false
		for _, want := range NonDispatchableBeadTypes {
			if want == typ {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("NonDispatchableBeadWispTypes()[%d] = %q, not in NonDispatchableBeadTypes", i, typ)
		}
		if typ == "wisp" {
			t.Errorf("NonDispatchableBeadWispTypes() includes %q, want it excluded from the wisp scan", typ)
		}
	}
	if want := len(NonDispatchableBeadTypes) - 1; len(got) != want {
		t.Errorf("NonDispatchableBeadWispTypes() returned %d items, want %d", len(got), want)
	}
}

func TestNonDispatchableBeadTypesLabels(t *testing.T) {
	// The representative mail case: a mail bead is typed "task" and distinguished
	// only by its "gt:message" label, so both halves must carry the message
	// family.
	if !containsString(NonDispatchableBeadTypes, "message") {
		t.Error("NonDispatchableBeadTypes missing \"message\"")
	}
	if !containsString(NonDispatchableBeadLabels, "gt:message") {
		t.Error("NonDispatchableBeadLabels missing \"gt:message\"")
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMayorRigsPath(t *testing.T) {
	got := MayorRigsPath("/town")
	expect := "/town/mayor/rigs.json"
	if got != expect {
		t.Errorf("MayorRigsPath = %q, want %q", got, expect)
	}
}

func TestMayorTownPath(t *testing.T) {
	got := MayorTownPath("/town")
	expect := "/town/mayor/town.json"
	if got != expect {
		t.Errorf("MayorTownPath = %q, want %q", got, expect)
	}
}

func TestRigMayorPath(t *testing.T) {
	got := RigMayorPath("/rig")
	expect := "/rig/mayor/rig"
	if got != expect {
		t.Errorf("RigMayorPath = %q, want %q", got, expect)
	}
}

func TestRigBeadsPath(t *testing.T) {
	got := RigBeadsPath("/rig")
	expect := "/rig/mayor/rig/.beads"
	if got != expect {
		t.Errorf("RigBeadsPath = %q, want %q", got, expect)
	}
}

func TestRigPolecatsPath(t *testing.T) {
	got := RigPolecatsPath("/rig")
	expect := "/rig/polecats"
	if got != expect {
		t.Errorf("RigPolecatsPath = %q, want %q", got, expect)
	}
}

func TestRigCrewPath(t *testing.T) {
	got := RigCrewPath("/rig")
	expect := "/rig/crew"
	if got != expect {
		t.Errorf("RigCrewPath = %q, want %q", got, expect)
	}
}

func TestMayorConfigPath(t *testing.T) {
	got := MayorConfigPath("/town")
	expect := "/town/mayor/config.json"
	if got != expect {
		t.Errorf("MayorConfigPath = %q, want %q", got, expect)
	}
}

func TestTownRuntimePath(t *testing.T) {
	got := TownRuntimePath("/town")
	expect := "/town/.runtime"
	if got != expect {
		t.Errorf("TownRuntimePath = %q, want %q", got, expect)
	}
}

func TestRigRuntimePath(t *testing.T) {
	got := RigRuntimePath("/rig")
	expect := "/rig/.runtime"
	if got != expect {
		t.Errorf("RigRuntimePath = %q, want %q", got, expect)
	}
}

func TestRigSettingsPath(t *testing.T) {
	got := RigSettingsPath("/rig")
	expect := "/rig/settings"
	if got != expect {
		t.Errorf("RigSettingsPath = %q, want %q", got, expect)
	}
}

func TestMayorAccountsPath(t *testing.T) {
	got := MayorAccountsPath("/town")
	expect := "/town/mayor/accounts.json"
	if got != expect {
		t.Errorf("MayorAccountsPath = %q, want %q", got, expect)
	}
}

func TestMayorQuotaPath(t *testing.T) {
	got := MayorQuotaPath("/town")
	expect := "/town/mayor/quota.json"
	if got != expect {
		t.Errorf("MayorQuotaPath = %q, want %q", got, expect)
	}
}

// TestTestSocketName pins the shape the doctor's tmux-test-socket check reads:
// the owning pid is the trailing field, and the name is unique per call.
func TestTestSocketName(t *testing.T) {
	name := TestSocketName("gt-test-tmux")
	if !strings.HasPrefix(name, "gt-test-tmux-") {
		t.Fatalf("TestSocketName() = %q, want the gt-test-tmux- prefix", name)
	}
	if !strings.HasSuffix(name, fmt.Sprintf("-%d", os.Getpid())) {
		t.Errorf("TestSocketName() = %q, want it to end with the owning pid %d", name, os.Getpid())
	}
	if again := TestSocketName("gt-test-tmux"); again == name {
		t.Errorf("two calls returned the same name %q", name)
	}
	if strings.ContainsAny(name, "/ .") {
		t.Errorf("TestSocketName() = %q, want no character tmux rejects in a socket name", name)
	}
}
