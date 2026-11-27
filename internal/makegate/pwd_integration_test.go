//go:build integration

package makegate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationMakeKeepsASymlinkedPwd runs a real make from a directory
// reached through a symlink, with PWD naming that link, the way a seat's shell
// carries /sandbox/gt/work. Go's build cache key holds the working path, so
// the checkout Makefile must leave such a PWD alone and seats share one cache
// key (gt-z12wep).
// Mutation: make the export unconditional and the recipe prints the real path.
func TestIntegrationMakeKeepsASymlinkedPwd(t *testing.T) {
	_, link := linkedDir(t)

	seen := printPwd(t, link, link)
	if seen.makePwd != link {
		t.Errorf("make's PWD = %q, want the caller's link %q", seen.makePwd, link)
	}
	if seen.childPwd != link {
		t.Errorf("the child's PWD = %q, want the link %q unchanged", seen.childPwd, link)
	}
}

// TestIntegrationMakeExportsThePhysicalPwd runs the same real make from the
// symlinked directory with PWD naming some other directory, the stale value a
// seat inherits when a shell moves. The export must hand children the physical
// path, so no child runs under a PWD that is not its working directory
// (gt-15ix74).
// Mutation: delete the export and the recipe prints the other directory.
func TestIntegrationMakeExportsThePhysicalPwd(t *testing.T) {
	real, link := linkedDir(t)
	other := filepath.Join(filepath.Dir(real), "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}
	// CURDIR is fully resolved; t.TempDir may itself sit under a symlink.
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("resolve real: %v", err)
	}

	seen := printPwd(t, link, other)
	if seen.makePwd != want {
		t.Errorf("make's PWD = %q, want the physical %q", seen.makePwd, want)
	}
}

// linkedDir returns t's temp directory holding a fresh directory and a symlink
// to it, the seat's shape: make runs in the link, CURDIR is the physical path.
func linkedDir(t *testing.T) (real, link string) {
	t.Helper()
	dir := t.TempDir()
	real = filepath.Join(dir, "real")
	link = filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return real, link
}

// pwdSeen is what one recipe saw: makePwd is $(PWD), the value make exports to
// its children; childPwd is what the recipe's shell carries.
type pwdSeen struct{ makePwd, childPwd string }

// printPwd runs a real make in dir with PWD=pwd; a throwaway makefile includes
// the checkout Makefile under test and adds the echo target. The included
// parse-time $(shell ...) probes complain on stderr, so only stdout is read.
func printPwd(t *testing.T, dir, pwd string) pwdSeen {
	t.Helper()
	makefile := filepath.Join(dir, "pwd.mk")
	body := "include " + repoRoot(t) + "/Makefile\n\n.PHONY: print-pwd\nprint-pwd:\n" +
		"\t@echo make=$(PWD)\n\t@echo child=$$PWD\n"
	if err := os.WriteFile(makefile, []byte(body), 0o644); err != nil {
		t.Fatalf("write makefile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "-f", makefile, "print-pwd")
	cmd.Dir = dir
	cmd.Env = envWithPwd(pwd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("make print-pwd with PWD=%s: %v\n%s", pwd, err, stderr.String())
	}

	var seen pwdSeen
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if v, ok := strings.CutPrefix(line, "make="); ok {
			seen.makePwd = v
		} else if v, ok := strings.CutPrefix(line, "child="); ok {
			seen.childPwd = v
		}
	}
	return seen
}

// envWithPwd is this process's environment with PWD replaced by pwd. A
// duplicate PWD entry would let the child read either value, so the inherited
// one is dropped.
func envWithPwd(pwd string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PWD=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "PWD="+pwd)
}
