package taskmap

import (
	"strings"
	"testing"
)

// TestConfigMigrateUseOpensWithRequiredOpener pins the config migrate Use to
// the opener every Use line takes, "Use this when" or "Use this to", so an
// agent reading the task map meets the situation first (gt-52jcpy).
func TestConfigMigrateUseOpensWithRequiredOpener(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("config migrate")
	if !ok {
		t.Fatal(`Lookup("config migrate") found no entry`)
	}
	if !strings.HasPrefix(e.Use, "Use this when") && !strings.HasPrefix(e.Use, "Use this to") {
		t.Errorf("config migrate Use = %q; want a line starting \"Use this when\" or \"Use this to\" that names the situation", e.Use)
	}
}
