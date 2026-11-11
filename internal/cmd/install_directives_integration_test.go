//go:build integration

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/testutil/directivegit"
)

// An install over a town whose gastown rig holds a real bare repo deploys the
// directives at the commit it is given, and an unknown commit is a warning
// that names it while the install still succeeds (gt-3lvcjz.13).
func TestIntegrationInstallDirectivesFromTheGastownRepo(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	repo := directivegit.NewBare(t, config.CentralDirectiveRepo(hqPath))
	commit := directivegit.Commit(t, repo, "", map[string]directivegit.Entry{
		"_common.md": {Content: "# common\n"},
	})
	const unknown = "1234567890abcdef1234567890abcdef12345678"

	var warn bytes.Buffer
	deployAt := func(c string) installSteps {
		return installSteps{deployDirectives: func(townRoot string) ([]string, error) {
			return config.DeployCentralDirectives(townRoot, c, nil)
		}}
	}
	if err := runInstallWith(installOptions{noBeads: true, warn: &warn, steps: deployAt(commit)}, []string{hqPath}); err != nil {
		t.Fatalf("gt install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(hqPath, "directives", "_common.md"))
	if err != nil {
		t.Fatalf("read the deployed directive: %v", err)
	}
	if string(got) != "# common\n" {
		t.Errorf("deployed _common.md = %q, want the commit's bytes", got)
	}
	if strings.Contains(warn.String(), "Could not") {
		t.Errorf("warning on a good commit = %q, want none", warn.String())
	}

	warn.Reset()
	if err := runInstallWith(installOptions{noBeads: true, force: true, warn: &warn, steps: deployAt(unknown)}, []string{hqPath}); err != nil {
		t.Fatalf("gt install --force at an unknown commit: %v, want the install to succeed", err)
	}
	if !strings.Contains(warn.String(), unknown) {
		t.Errorf("warning = %q, want it to name the unknown commit %s", warn.String(), unknown)
	}
}
