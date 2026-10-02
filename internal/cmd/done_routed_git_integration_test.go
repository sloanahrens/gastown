//go:build integration

package cmd

// gitOut runs real git, so it lives behind the integration tag (the unit tier
// runs no git; see internal/testpolicy realgit.txt).

import (
	"os/exec"
	"strings"
	"testing"
)

// gitOut runs git in dir and returns its trimmed stdout, failing the test on
// error. The cmd tier's gt done integration tests use it to inspect the repo
// gt done ran in.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}
