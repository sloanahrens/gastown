//go:build integration

package shelltier

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntegrationOSExecReportsTheExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		want int
	}{
		{name: "passing", argv: []string{"true"}, want: 0},
		{name: "failing", argv: []string{"false"}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			code, err := OSExec(t.Context(), Command{
				Dir:  dir,
				Argv: tc.argv,
				Log:  filepath.Join(dir, "logs", "run.log"),
			})
			if err != nil {
				t.Fatalf("OSExec error: %v", err)
			}
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestIntegrationRunnerEndToEndOnATwoScriptFixture(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scripts := map[string]string{
		"pass_test.sh": "exit 0\n",
		"fail_test.sh": "echo boom\nexit 1\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logDir := t.TempDir()

	r := Run(t.Context(), Config{
		Entries:     entriesAt("pass_test.sh", "fail_test.sh"),
		Root:        root,
		LogDir:      logDir,
		Budget:      time.Minute,
		Exec:        OSExec,
		Now:         time.Now,
		WithTimeout: context.WithTimeout,
	})

	want := "tier-sweep: shell RED passed=1 failed=1 skipped=0 failed: fail_test.sh (logs " + logDir + ")"
	if line := Summary(r, logDir); !strings.HasPrefix(line, want) {
		t.Fatalf("summary = %q, want prefix %q", line, want)
	}
	if got := r.Exit(); got != ExitRed {
		t.Fatalf("exit = %d, want ExitRed", got)
	}
	if _, err := os.Stat(filepath.Join(logDir, "shell-fail_test.sh.log")); err != nil {
		t.Fatalf("failing script's log not written: %v", err)
	}
}
