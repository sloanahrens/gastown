package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationLandIsGone: gt mq integration land was an unreviewed route
// to main that never landed anything (d2 evidence Q1: 0 landings). It is
// deleted (gt-fcxe9.4); create and status stay, and nothing the refinery
// reads tells it to run the command.
func TestIntegrationLandIsGone(t *testing.T) {
	t.Parallel()
	integration := findCommand(t, "gt mq integration")
	for _, sub := range integration.Commands() {
		if sub.Name() == "land" {
			t.Fatal("gt mq integration land is still in the command tree")
		}
	}
	findCommand(t, "gt mq integration create")
	findCommand(t, "gt mq integration status")

	for _, rel := range []string{
		"internal/formula/formulas/mol-refinery-patrol.formula.toml",
		"internal/templates/roles/refinery.md.tmpl",
		".githooks/pre-push",
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"mq integration land", "GT_INTEGRATION_LAND=1 to bypass", `"$GT_INTEGRATION_LAND" != "1"`} {
			if strings.Contains(string(data), banned) {
				t.Errorf("%s still contains %q", rel, banned)
			}
		}
	}
}
