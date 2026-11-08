package townconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ValidateSettingsDocument refuses the documents the kernel load refuses, and
// names the file in every error, so a writer can refuse them before saving.
func TestValidateSettingsDocumentRefusesWhatTheLoadRefuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an unknown key",
			body: `{"type":"town-settings","version":1,"nope":true}`,
			want: "unknown key",
		},
		{
			name: "the wrong type",
			body: `{"type":"town","version":1}`,
			want: "expected type 'town-settings'",
		},
		{
			name: "a later version",
			body: `{"type":"town-settings","version":99}`,
			want: "unsupported config version",
		},
		{
			name: "a pool count below zero",
			body: `{"type":"town-settings","version":1,"polecat_pool":{"max_priority":-1}}`,
			want: "must be >= 0",
		},
		{
			name: "a seat for an agent that does not resolve",
			body: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"nobody":{"cap":1}}}}`,
			want: "no agent of that name",
		},
		{
			name: "a literal token the town refuses",
			body: `{"type":"town-settings","version":1,
			        "agents":{"worker":{"command":"claude","env":{"API_TOKEN":"sk-ant-0123456789abcdef"}}},
			        "secrets":{"refuse_literals":true}}`,
			want: "a literal token",
		},
		{
			name: "a scheduled run_agent with no seat",
			body: `{"type":"town-settings","version":1,
			        "agents":{"claude-sonnet":{"command":"claude"}},
			        "polecat_pool":{"seats":{"claude-sonnet":{"cap":1}}},
			        "daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_slings":{"enabled":true,
			          "entries":[{"name":"deep-review","rig":"gastown","formula":"mol-deep-review","interval":"1h","dispatch":"spec","run_agent":"claude-opus"}]}}}}`,
			want: `scheduled_slings[deep-review].run_agent "claude-opus": no seat for that agent in polecat_pool.seats`,
		},
		{
			name: "a scheduled run_agent without dispatch spec",
			body: `{"type":"town-settings","version":1,
			        "agents":{"claude-sonnet":{"command":"claude"}},
			        "polecat_pool":{"seats":{"claude-sonnet":{"cap":1}}},
			        "daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_slings":{"enabled":true,
			          "entries":[{"name":"deep-review","rig":"gastown","formula":"mol-deep-review","interval":"1h","run_agent":"claude-sonnet"}]}}}}`,
			want: `scheduled_slings[deep-review].run_agent: valid only with dispatch "spec"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := ValidateSettingsDocument(FileSettings, []byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ValidateSettingsDocument = %v, want %q", err, c.want)
			} else if !strings.Contains(err.Error(), FileSettings) {
				t.Errorf("error = %v, want the file named", err)
			}
		})
	}
}

// The town the load accepts validates, and its seats come back resolved.
func TestValidateSettingsDocumentAcceptsTheFixtureTown(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(fixtureTown, FileSettings))
	if err != nil {
		t.Fatal(err)
	}
	settings, seats, err := ValidateSettingsDocument(FileSettings, data)
	if err != nil {
		t.Fatalf("ValidateSettingsDocument = %v, want the fixture accepted", err)
	}
	if settings == nil {
		t.Fatal("settings = nil, want the document's")
	}
	if len(seats) == 0 {
		t.Error("seats = none, want the fixture's seat map")
	}
}
