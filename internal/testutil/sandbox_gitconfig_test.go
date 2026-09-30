package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox gitconfig gives tests a git identity, turns off git's fsync
// (nothing a test writes outlives the test binary, and the flushes serialise
// parallel git-heavy tests, gt-22hdp.33), and points init at a minimal
// template: hooks/ and info/exclude, which code under test may write into,
// without the stock template's inert sample hooks. That git reads it so is
// TestIntegrationSandboxGitConfig*'s.
func TestSandboxGitConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := writeSandboxGitConfig(home); err != nil {
		t.Fatalf("writeSandboxGitConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(home, ".git-template")
	for _, want := range []string{
		"[user]\n\tname = Hermetic Test\n\temail = hermetic@test.invalid",
		"[init]\n\tdefaultBranch = main\n\ttemplateDir = " + template + "\n",
		"[core]\n\tfsync = none",
		"[commit]\n\tgpgsign = false",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf(".gitconfig lacks %q:\n%s", want, data)
		}
	}
	entries, err := os.ReadDir(template)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "hooks info" {
		t.Errorf("template holds %q, want hooks and info only", names)
	}
	if hooks, _ := os.ReadDir(filepath.Join(template, "hooks")); len(hooks) != 0 {
		t.Errorf("template hooks/ is not empty: %d entries", len(hooks))
	}
	if _, err := os.Stat(filepath.Join(template, "info", "exclude")); err != nil {
		t.Errorf("template has no info/exclude: %v", err)
	}
}
