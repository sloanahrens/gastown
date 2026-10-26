//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unit tests drive the runner seam, which cannot show that the go tool
// accepts the argv the command builds or that the module directory resolves
// the packages. These two run the real binary on a fixture module under the
// integration tier, where a subprocess is allowed (gt-w3aby).

// writeModule writes files under a fresh module directory.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module example.invalid/fixture\n\ngo 1.21\n"
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestIntegrationReportsAnIntegrationOnlyTypeError is the motivating case: a
// file that exists only under the integration tag and does not type-check. The
// unit tier cannot see it, so the gate's verdict has to come from here.
func TestIntegrationReportsAnIntegrationOnlyTypeError(t *testing.T) {
	root := writeModule(t, map[string]string{
		"tagged/ok.go":   "//go:build integration\n\npackage tagged\n\nfunc OK() int { return 1 }\n",
		"broken/b.go":    "//go:build integration\n\npackage broken\n\nvar _ = missing\n",
		"plain/plain.go": "package plain\n",
	})
	var out bytes.Buffer

	code := run(context.Background(), []string{"-root", root}, &out)

	if code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "undefined: missing") {
		t.Errorf("go vet's type error is missing from the output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAILED in 1 of 1 modules") {
		t.Errorf("output does not report the failing module:\n%s", out.String())
	}
}

// TestIntegrationPassesOnIntegrationCodeThatTypeChecks pins the other half: the
// real go tool exits 0 on a selection that builds, so the gate's green verdict
// is not vacuous.
func TestIntegrationPassesOnIntegrationCodeThatTypeChecks(t *testing.T) {
	root := writeModule(t, map[string]string{
		"tagged/ok.go": "//go:build integration\n\npackage tagged\n\nfunc OK() int { return 1 }\n",
	})
	var out bytes.Buffer

	code := run(context.Background(), []string{"-root", root}, &out)

	if code != 0 {
		t.Fatalf("run = %d, want 0; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 packages in 1 modules type-check with -tags integration") {
		t.Errorf("output does not report the vetted package:\n%s", out.String())
	}
}
