//go:build integration

package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/buildsrc"
)

// gitInTest runs git in dir with a fixed identity and no signing.
func gitInTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The same dry run with --clean must say what it would remove, not just name
// the plugin. The source once shipped seat-refill, so the real run removes the
// runtime copy; without that history it would protect it as a runtime edit.
func TestIntegrationRunPluginSync_DryRunCleanSaysWouldRemove(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	sourcePlugins := filepath.Join(buildCheckoutDir(townRoot), "plugins")
	writeTestPlugin(t, sourcePlugins, "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "seat-refill", "retired")
	writeTestPlugin(t, sourcePlugins, "seat-refill", "retired")
	gitInTest(t, sourcePlugins, "init", "-q")
	gitInTest(t, sourcePlugins, "add", "-A")
	gitInTest(t, sourcePlugins, "commit", "-q", "-m", "ship seat-refill")
	if err := os.RemoveAll(filepath.Join(sourcePlugins, "seat-refill")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	r := pluginSyncRun{ctx: context.Background(), townRoot: townRoot, build: fixedBuild(buildCheckoutDir(townRoot)), dryRun: true, clean: true, out: &out, errOut: io.Discard}
	if err := r.run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), "seat-refill (would be removed)") {
		t.Errorf("--clean dry run did not name what it would remove; output:\n%s", out.String())
	}
}

// gitOutputInTest runs git in dir and returns its trimmed stdout.
func gitOutputInTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// gt-i3rotl: a dry run over an existing build checkout reads it and leaves it
// where it is. The bare repo's main has moved past the checkout, which is the
// commit buildsrc.Ensure would move it to, so a dry run that reached Ensure
// would change HEAD and the refs. Built with real git on a temporary town.
func TestIntegrationRunPluginSync_DryRunLeavesExistingCheckoutAlone(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)

	// The rig's source repo: plugins at commit A, then commit B on main.
	src := t.TempDir()
	gitInTest(t, src, "init", "-b", "main")
	writeTestPlugin(t, filepath.Join(src, "plugins"), "current-plugin", "A")
	gitInTest(t, src, "add", "-A")
	gitInTest(t, src, "commit", "-m", "A")
	commitA := gitOutputInTest(t, src, "rev-parse", "HEAD")

	// The bare repo the build checkout clones from, holding A as origin/main.
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0755); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, townRoot, "clone", "--bare", src, bare)
	gitInTest(t, bare, "update-ref", "refs/remotes/origin/main", commitA)

	checkout := buildCheckoutDir(townRoot)
	if err := os.MkdirAll(filepath.Dir(checkout), 0755); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, townRoot, "clone", bare, checkout)

	writeTestPlugin(t, filepath.Join(src, "plugins"), "current-plugin", "B")
	gitInTest(t, src, "add", "-A")
	gitInTest(t, src, "commit", "-m", "B")
	commitB := gitOutputInTest(t, src, "rev-parse", "HEAD")
	gitInTest(t, src, "push", bare, "main")
	gitInTest(t, bare, "update-ref", "refs/remotes/origin/main", commitB)

	headBefore := gitOutputInTest(t, checkout, "rev-parse", "HEAD")
	refsBefore := gitOutputInTest(t, checkout, "for-each-ref", "--format=%(refname) %(objectname)")
	if headBefore != commitA {
		t.Fatalf("checkout HEAD = %s before the dry run, want commit A %s", headBefore, commitA)
	}

	var out bytes.Buffer
	dry := pluginSyncRun{ctx: context.Background(), townRoot: townRoot, build: buildsrc.Ensure, dryRun: true, out: &out, errOut: io.Discard}
	if err := dry.run(); err != nil {
		t.Fatalf("dry run error = %v; output:\n%s", err, out.String())
	}

	if got := gitOutputInTest(t, checkout, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("dry run moved the checkout's HEAD from %s to %s", headBefore, got)
	}
	if got := gitOutputInTest(t, checkout, "for-each-ref", "--format=%(refname) %(objectname)"); got != refsBefore {
		t.Errorf("dry run changed the checkout's refs:\nbefore:\n%s\nafter:\n%s", refsBefore, got)
	}
	if want := "  Source: " + filepath.Join(checkout, "plugins") + " (gastown build checkout)"; !strings.Contains(out.String(), want) {
		t.Errorf("dry run did not read the existing checkout; want line %q, output:\n%s", want, out.String())
	}
}
