package rigbootstrap

import (
	"bytes"
	"os"
	"testing"
)

func TestMergeRoleAgentsFromNothing(t *testing.T) {
	t.Parallel()
	got, err := MergeRoleAgents(nil, "claude")
	if err != nil {
		t.Fatalf("MergeRoleAgents(nil, claude) error = %v", err)
	}
	want := "{\n" +
		"  \"type\": \"rig-settings\",\n" +
		"  \"version\": 1,\n" +
		"  \"role_agents\": {\n" +
		"    \"polecat\": \"claude\"\n" +
		"  }\n" +
		"}\n"
	if string(got) != want {
		t.Fatalf("MergeRoleAgents(nil, claude) =\n%s\nwant\n%s", got, want)
	}
}

func TestMergeRoleAgentsKeepsWhatIsThere(t *testing.T) {
	t.Parallel()
	before := readFixture(t, "settings_before.json")
	after := readFixture(t, "settings_after.json")

	got, err := MergeRoleAgents(before, "claude")
	if err != nil {
		t.Fatalf("MergeRoleAgents(before, claude) error = %v", err)
	}
	if !bytes.Equal(got, after) {
		t.Fatalf("MergeRoleAgents(before, claude) =\n%s\nwant\n%s", got, after)
	}

	// The merged file is a fixed point: merging it again changes nothing.
	again, err := MergeRoleAgents(got, "claude")
	if err != nil {
		t.Fatalf("MergeRoleAgents(after, claude) error = %v", err)
	}
	if !bytes.Equal(again, after) {
		t.Fatalf("MergeRoleAgents(after, claude) =\n%s\nwant\n%s", again, after)
	}
}

func TestMergeRoleAgentsEmptyPolecatKeepsRoleAgents(t *testing.T) {
	t.Parallel()
	before := readFixture(t, "settings_before.json")

	got, err := MergeRoleAgents(before, "")
	if err != nil {
		t.Fatalf("MergeRoleAgents(before, \"\") error = %v", err)
	}
	if !bytes.Contains(got, []byte(`"polecat": "codex"`)) {
		t.Fatalf("MergeRoleAgents(before, \"\") lost role_agents.polecat:\n%s", got)
	}
}

func TestMergeRoleAgentsBadJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing string
	}{
		{name: "not JSON", existing: "not json"},
		{name: "truncated object", existing: `{"type": "rig-settings"`},
		{name: "value is not JSON", existing: `{"type": }`},
		{name: "object closed by a bracket", existing: `{"type": "rig-settings"]`},
		{name: "whitespace only", existing: "  \n"},
		{name: "top level array", existing: `[]`},
		{name: "data after the object", existing: `{} {}`},
		{name: "role_agents is not an object", existing: `{"role_agents": "claude"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MergeRoleAgents([]byte(tt.existing), "claude")
			if err == nil {
				t.Fatalf("MergeRoleAgents(%q) = %s, want an error", tt.existing, got)
			}
			if got != nil {
				t.Fatalf("MergeRoleAgents(%q) returned bytes with its error: %s", tt.existing, got)
			}
		})
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}
