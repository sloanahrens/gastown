package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// testEnv is a process whose install tree is a temporary directory and which
// has no town above its working directory, so a run never reads or writes the
// host's install directory.
func testEnv(t *testing.T) (environment, string) {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"INSTALL_GT_BIN_DIR":    filepath.Join(root, "bin"),
		"INSTALL_GT_DAEMON_DIR": filepath.Join(root, "daemon"),
		"INSTALL_GT_RIG_DIR":    filepath.Join(root, "rig"),
	}
	env := environment{
		getenv: func(key string) string { return values[key] },
		getwd:  func() (string, error) { return root, nil },
		home:   func() (string, error) { return root, nil },
	}
	return env, root
}

// A bad argument is reported before any directory is resolved: the message on
// stderr, then the guard's result line, status 1.
func TestRunWithABadArgument(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		args       []string
		wantLast   string
		wantStderr string
	}{
		{
			name:       "an empty --sha names nothing",
			args:       []string{"--sha", ""},
			wantLast:   "install-gt: RESULT failed - - unexpected",
			wantStderr: "install-gt: --sha needs a commit",
		},
		{
			name:       "a bad source still ends in a result line",
			args:       []string{"--sha", "abc123", "--source", "bogus"},
			wantLast:   "install-gt: RESULT failed abc123 - unexpected",
			wantStderr: "install-gt: --source must be manual, rebuild-gt or post-land",
		},
		{
			name:       "an unknown argument stops parsing where it stands",
			args:       []string{"--bogus", "--sha", "abc123"},
			wantLast:   "install-gt: RESULT failed origin/main - unexpected",
			wantStderr: "install-gt: unknown argument: --bogus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := testEnv(t)
			var out, errOut strings.Builder

			code := runWith(tc.args, &out, &errOut, env)

			if code != 1 {
				t.Errorf("run(%q) = %d, want 1", tc.args, code)
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != 1 || lines[0] != tc.wantLast {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantLast)
			}
			if got := strings.TrimSuffix(errOut.String(), "\n"); got != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tc.wantStderr)
			}
		})
	}
}

// The first section reports for itself: no build checkout is a failed run with
// the script's log line, result line and receipt, and the binary is untouched.
func TestRunWithoutABuildCheckout(t *testing.T) {
	t.Parallel()
	env, root := testEnv(t)
	var out, errOut strings.Builder

	code := runWith(nil, &out, &errOut, env)

	if code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	rig := filepath.Join(root, "rig")
	want := "[install-gt] No build checkout at " + rig + "\n" +
		"install-gt: RESULT failed origin/main - no-rig\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
	receipts, err := os.ReadFile(filepath.Join(root, "daemon", "install-receipts.jsonl"))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if !strings.Contains(string(receipts), `"reason": "no-rig"`) {
		t.Errorf("receipts = %q, want reason no-rig", receipts)
	}
}

// --dry-run changes nothing but the receipt the exit contract always leaves,
// and until the gateway step lands (gt-tpg0p.9) it ends in the guard: the
// checkout exists but is never read.
func TestRunDryRunTouchesNothing(t *testing.T) {
	t.Parallel()
	env, root := testEnv(t)
	if err := os.MkdirAll(filepath.Join(root, "rig", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	var out, errOut strings.Builder

	code := runWith([]string{"--dry-run"}, &out, &errOut, env)

	if code != 1 {
		t.Errorf("run(--dry-run) = %d, want 1", code)
	}
	if want := "install-gt: RESULT failed origin/main - unexpected\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	after := snapshot(t, root)
	receipts := filepath.Join("daemon", "install-receipts.jsonl")
	content, ok := after[receipts]
	if !ok {
		t.Fatalf("no receipt at %s", receipts)
	}
	// The receipt writer creates the daemon directory the way the script's
	// mkdir -p does; that directory and the receipt are the whole change.
	delete(after, receipts)
	delete(after, "daemon")
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the tree changed:\nbefore %v\nafter  %v", before, after)
	}
	if !strings.Contains(content, `"reason": "unexpected"`) {
		t.Errorf("receipts = %q, want the guard's unexpected receipt", content)
	}
}

// snapshot is every path under root, with a file's contents, so a test can say
// what a run changed.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return tree
}
