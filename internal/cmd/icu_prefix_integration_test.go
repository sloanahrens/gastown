//go:build integration || e2e

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/buildflags"
)

// TestIntegrationBrewIcu4cPrefixSkipsUninstalledFormula covers a brew that
// names an uninstalled icu4c (icu4c@79) while icu4c@78 holds the headers. The
// fake brew stands in for the host, so the test does not depend on what is
// installed here.
func TestIntegrationBrewIcu4cPrefixSkipsUninstalledFormula(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "opt", "icu4c@78")
	if err := os.MkdirAll(filepath.Join(installed, "include", "unicode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "include", "unicode", "regex.h"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBrew := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--prefix\" ] && [ \"$2\" = \"icu4c\" ]; then echo " + filepath.Join(root, "opt", "icu4c@79") + "; exit 0; fi\n" +
		"if [ \"$1\" = \"--prefix\" ] && [ -z \"$2\" ]; then echo " + root + "; exit 0; fi\n" +
		"exit 1\n"
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "brew"), []byte(fakeBrew), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := buildflags.BrewICU4CPrefix(); got != installed {
		t.Fatalf("BrewICU4CPrefix() = %q, want the installed keg %q", got, installed)
	}
}
