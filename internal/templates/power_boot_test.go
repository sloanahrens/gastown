package templates

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// TestPowerBootTemplate pins the login job gt power documents: the plist runs
// "power boot-hold" for the town it is rendered with, at load.
func TestPowerBootTemplate(t *testing.T) {
	t.Parallel()
	raw, err := supervisorFS.ReadFile("launchd/com.gastown.power-boot.plist")
	if err != nil {
		t.Fatalf("reading the power-boot template: %v", err)
	}
	tmpl, err := template.New("power-boot").Parse(string(raw))
	if err != nil {
		t.Fatalf("parsing the power-boot template: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, SupervisorData{GTPath: "/usr/local/bin/gt", TownRoot: "/town"}); err != nil {
		t.Fatalf("rendering the power-boot template: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"<string>com.gastown.power-boot</string>",
		"<string>/usr/local/bin/gt</string>",
		"<string>power</string>",
		"<string>boot-hold</string>",
		"<key>RunAtLoad</key>",
		"<string>/town</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered power-boot plist missing %q:\n%s", want, got)
		}
	}
}
