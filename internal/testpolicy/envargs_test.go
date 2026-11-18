package testpolicy

import (
	"os"
	"path/filepath"
	"testing"
)

// envArgsSnippet wraps body in a test file the scanner can parse. The file is
// never compiled, so an import the body does not use is harmless.
func envArgsSnippet(body string) string {
	return "package p\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestX(t *testing.T) {\n" + body + "\n}\n"
}

// envArgsRoot returns a temp repo root holding the package and command trees
// the scan walks.
func envArgsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"internal/p", "cmd"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// scanEnvArgs writes src as a test file of an internal package and scans it.
func scanEnvArgs(t *testing.T, src string) []Violation {
	t.Helper()
	root := envArgsRoot(t)
	if err := os.WriteFile(filepath.Join(root, "internal", "p", "p_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := ScanEnvArgs(root)
	if err != nil {
		t.Fatalf("ScanEnvArgs: %v", err)
	}
	return vs
}

func TestEnvArgsRule(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body string
		want int
	}{
		"fatalf identifier":                   {`t.Fatalf("%v", env)`, 1},
		"errorf identifier":                   {`t.Errorf("env = %v", env)`, 1},
		"logf identifier":                     {`t.Logf("%v", env)`, 1},
		"environ call":                        {`t.Fatalf("%v", os.Environ())`, 1},
		"sprintf identifier":                  {`t.Errorf("%s", fmt.Sprintf("%v", env))`, 1},
		"message names a key only":            {`t.Errorf("env has %d entries", 2)`, 0},
		"another variable":                    {`got := []string{"A=1"}; t.Errorf("%v", got)`, 0},
		"field is out of scope":               {`t.Errorf("%v", cmd.Env)`, 0},
		"index is out of scope":               {`t.Errorf("%v", env[0])`, 0},
		"marker on the call":                  {`t.Errorf("%v", env) // testpolicy:env-print-ok an endpoint, not the environment`, 0},
		"marker does not cover next line":     {`t.Errorf("%v", env) // testpolicy:env-print-ok an endpoint` + "\n\t" + `t.Errorf("%v", env)`, 1},
		"marker exempts one call on its line": {`t.Errorf("%v", env); t.Fatalf("%v", env) // testpolicy:env-print-ok one endpoint`, 1},
		"marker exempts the call it trails":   {`t.Errorf("%v", env); t.Errorf("ok") // testpolicy:env-print-ok an endpoint`, 1},
		"marker exempts the last call":        {`t.Errorf("ok"); t.Errorf("%v", env) // testpolicy:env-print-ok an endpoint`, 0},
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := len(scanEnvArgs(t, envArgsSnippet(c.body))); got != c.want {
				t.Fatalf("violations = %d, want %d", got, c.want)
			}
		})
	}
}

func TestEnvArgsUnreadableRoot(t *testing.T) {
	t.Parallel()
	if _, err := ScanEnvArgs(t.TempDir()); err == nil {
		t.Fatal("ScanEnvArgs on a root with no internal/ returned no error")
	}
}

func TestEnvArgsBuildTaggedFile(t *testing.T) {
	t.Parallel()
	src := "//go:build integration\n\n" + envArgsSnippet(`t.Fatalf("%v", env)`)
	if got := len(scanEnvArgs(t, src)); got != 1 {
		t.Fatalf("violations = %d, want 1 in a build-tagged test file", got)
	}
}

func TestEnvArgsParseError(t *testing.T) {
	t.Parallel()
	root := envArgsRoot(t)
	broken := filepath.Join(root, "internal", "p", "broken_test.go")
	if err := os.WriteFile(broken, []byte("package p\nfunc ("), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanEnvArgs(root); err == nil {
		t.Fatal("ScanEnvArgs accepted an unparseable test file")
	}
}

func TestEnvArgsGuardOnTree(t *testing.T) {
	t.Parallel()
	vs, err := ScanEnvArgs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("ScanEnvArgs: %v", err)
	}
	for _, v := range vs {
		t.Error(v.String())
	}
}
