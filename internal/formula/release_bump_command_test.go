package formula

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// releaseBumpCommand is the line both release formulas run to bump the version.
const releaseBumpCommand = "go run ./cmd/bump-version {{version}}"

// releaseStep is the part of a formula step the bump check reads.
type releaseStep struct {
	ID          string `toml:"id"`
	Title       string `toml:"title"`
	Description string `toml:"description"`
}

// TestReleaseFormulasRunBumpCommand pins the bump step of each release formula to
// the Go command. A formula that still names the deleted bump-version.sh, or that
// runs a different command line, hands a releaser a step that cannot run.
func TestReleaseFormulasRunBumpCommand(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"gastown-release.formula.toml", "beads-release.formula.toml"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := GetEmbeddedFormulaContent(name)
			if err != nil {
				t.Fatalf("GetEmbeddedFormulaContent(%s): %v", name, err)
			}

			var formula struct {
				Steps []releaseStep `toml:"steps"`
			}
			if _, err := toml.Decode(string(data), &formula); err != nil {
				t.Fatalf("decode %s: %v", name, err)
			}

			var bump *releaseStep
			for i := range formula.Steps {
				if formula.Steps[i].ID == "run-bump-script" {
					bump = &formula.Steps[i]
					break
				}
			}
			if bump == nil {
				t.Fatalf("%s has no run-bump-script step", name)
			}

			if !strings.Contains(bump.Description, "\n"+releaseBumpCommand+"\n") {
				t.Errorf("%s run-bump-script does not run %q:\n%s", name, releaseBumpCommand, bump.Description)
			}
			if strings.Contains(bump.Title+bump.Description, "bump-version.sh") {
				t.Errorf("%s run-bump-script still names bump-version.sh", name)
			}
		})
	}
}
