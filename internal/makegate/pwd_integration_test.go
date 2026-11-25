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

// TestIntegrationMakeExportsThePhysicalPwd runs a real make from a directory
// reached through a symlink, with PWD naming the symlink, the way a seat's
// shell carries /sandbox/gt/work. The checkout Makefile is the one under
// test, included by a throwaway makefile that adds the echo target; its
// export must hand the recipe the physical path, not the caller's PWD.
// Mutation: delete the export line and the recipe prints the symlink.
func TestIntegrationMakeExportsThePhysicalPwd(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The physical path make reports is fully resolved: t.TempDir can itself
	// sit under a symlink (/var on macOS).
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("resolve real: %v", err)
	}

	makefile := filepath.Join(real, "pwd.mk")
	body := "include " + repoRoot(t) + "/Makefile\n\n.PHONY: print-pwd\nprint-pwd:\n\t@echo $$PWD\n"
	if err := os.WriteFile(makefile, []byte(body), 0o644); err != nil {
		t.Fatalf("write makefile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "-f", makefile, "print-pwd")
	cmd.Dir = link
	cmd.Env = envWithPwd(link)
	// The included Makefile's parse-time $(shell ...) probes look for files
	// beside the temp makefile and complain on stderr; only stdout carries the
	// recipe's PWD.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("make print-pwd: %v\n%s", err, stderr.String())
	}

	got := strings.TrimSpace(string(out))
	if got != want {
		t.Fatalf("the child's PWD = %q, want the physical %q", got, want)
	}
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
