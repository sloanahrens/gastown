//go:build integration

package done

// testRunGit is done's integration-tier git helper. The equivalent in
// internal/cmd is unexported to that package, and the leaf cannot import it
// (D10), so the moved tests carry their own copy. The unit tier runs no git
// (internal/testpolicy realgit.txt), so this lives behind the tag.

import (
	"os"
	"os/exec"
	"testing"
)

// writeTestFile mirrors the cmd tier's helper of the same name (cmd's is
// unexported to that package). writeTestFileAt lives with the revert-check
// test that uses it.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// runGitCmd mirrors cmd's git_helpers_integration_test.go helper.
func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func testRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	// An empty dir runs git in the test process's own cwd — the package
	// directory inside the real repo — so a stray test would create branches and
	// objects in the rig's shared ref store.
	if dir == "" {
		t.Fatal("testRunGit: empty dir would run git in the test process cwd")
	}
	fullArgs := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
