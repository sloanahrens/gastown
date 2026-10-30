package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files under a new temp directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runMain runs the command with the environment given and no git top level, and
// returns stdout, stderr and the exit code.
func runMain(t *testing.T, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, func(k string) string { return env[k] },
		func() (string, error) { return "", errors.New("no git in this test") }, &out, &errb)
	return out.String(), errb.String(), code
}

// TestMainOptions covers the command over a real directory: the root from
// DOCS_LINT_ROOT, the ceiling from DOCS_LINT_CEILING, the listings, the word
// total and the refusals. --slice needs real commit dates and is in the
// integration test.
func TestMainOptions(t *testing.T) {
	t.Parallel()
	root := writeTree(t, map[string]string{
		"AGENTS.md":         "# Agents\n\nfive words in this file\n",
		"internal/pkg/a.go": "package pkg\n",
	})
	env := map[string]string{"DOCS_LINT_ROOT": root}
	t.Run("clean tree", func(t *testing.T) {
		t.Parallel()
		out, errOut, code := runMain(t, env)
		if out != "" || errOut != "" || code != 0 {
			t.Errorf("stdout %q stderr %q exit %d, want silent exit 0", out, errOut, code)
		}
	})
	t.Run("list", func(t *testing.T) {
		t.Parallel()
		out, _, code := runMain(t, env, "--list", "agent-facing")
		if out != "AGENTS.md\n" || code != 0 {
			t.Errorf("--list agent-facing = %q exit %d", out, code)
		}
		out, _, code = runMain(t, env, "--list", "go")
		if out != "internal/pkg/a.go\n" || code != 0 {
			t.Errorf("--list go = %q exit %d", out, code)
		}
	})
	t.Run("words", func(t *testing.T) {
		t.Parallel()
		out, _, code := runMain(t, env, "--words")
		if out != "7\n" || code != 0 {
			t.Errorf("--words = %q exit %d, want 7", out, code)
		}
	})
	t.Run("ceiling", func(t *testing.T) {
		t.Parallel()
		low := map[string]string{"DOCS_LINT_ROOT": root, "DOCS_LINT_CEILING": "6"}
		out, _, code := runMain(t, low)
		if want := "AGENTS.md:1: word-ceiling: 7 words, ceiling 6\n"; out != want || code != 1 {
			t.Errorf("ceiling 6: %q exit %d, want %q exit 1", out, code, want)
		}
		bad := map[string]string{"DOCS_LINT_ROOT": root, "DOCS_LINT_CEILING": "many"}
		out, errOut, code := runMain(t, bad)
		if out != "" || code != 2 || !strings.Contains(errOut, "DOCS_LINT_CEILING 'many'") {
			t.Errorf("ceiling many: stdout %q stderr %q exit %d, want exit 2 naming it", out, errOut, code)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{{"--nope"}, {"--list"}, {"--slice", "1"}, {"--words", "x"}} {
			out, errOut, code := runMain(t, env, args...)
			if out != "" || code != 2 || !strings.HasPrefix(errOut, "docs-lint: ") {
				t.Errorf("%v: stdout %q stderr %q exit %d, want exit 2 with a docs-lint: message", args, out, errOut, code)
			}
		}
	})
	t.Run("no root", func(t *testing.T) {
		t.Parallel()
		out, errOut, code := runMain(t, nil)
		if out != "" || code != 2 || !strings.Contains(errOut, "cannot find the repository root") {
			t.Errorf("no root: stdout %q stderr %q exit %d, want exit 2", out, errOut, code)
		}
	})
}

// TestMainTopLevel checks the top level is what git printed, trimmed, and that a
// git that fails or prints nothing is an error rather than an empty root.
func TestMainTopLevel(t *testing.T) {
	t.Parallel()
	if got, err := topLevelFrom(func() ([]byte, error) { return []byte("/work/repo\n"), nil }); err != nil || got != "/work/repo" {
		t.Errorf("top level = %q, %v, want /work/repo", got, err)
	}
	boom := errors.New("not a git repository")
	if _, err := topLevelFrom(func() ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("a failing git: err = %v, want it to wrap the failure", err)
	}
	if got, err := topLevelFrom(func() ([]byte, error) { return []byte("\n"), nil }); err == nil {
		t.Errorf("empty output: top level %q and no error, want an error", got)
	}
}
