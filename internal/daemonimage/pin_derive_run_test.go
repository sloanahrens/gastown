package daemonimage

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestDeriveGoListArgsAreTheDevopsCommand: the guard runs exactly the derivation
// the devops workflow runs, through the runner seam, in the directory it is
// given; the production runner is a goListRunner.
func TestDeriveGoListArgsAreTheDevopsCommand(t *testing.T) {
	t.Parallel()
	var gotDir string
	var gotArgs []string
	fake := func(dir string, args ...string) ([]byte, error) {
		gotDir, gotArgs = dir, args
		return []byte("/repo/cmd/reporter\n"), nil
	}

	out, err := runGoList(fake, "/repo")

	if err != nil || string(out) != "/repo/cmd/reporter\n" {
		t.Fatalf("runGoList = %q, %v; want the runner's output", out, err)
	}
	if gotDir != "/repo" {
		t.Errorf("runner dir = %q, want /repo", gotDir)
	}
	want := []string{"list", "-deps", "-f", "{{.Dir}}", "./cmd/reporter", "./cmd/release-reporter", "./cmd/cloudcheck"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("runner args = %q, want %q", gotArgs, want)
	}
	var _ goListRunner = execGoList
}

// TestGoListFailureCarriesTheCallAndStderr: a failed derivation names the
// command, the directory, the exit error and go's own stderr text, and the exit
// error stays matchable.
func TestGoListFailureCarriesTheCallAndStderr(t *testing.T) {
	t.Parallel()
	exit := errors.New("exit status 1")

	err := goListFailure("/repo", deriveGoListArgs(), exit, "go: cannot find main module\n")

	if !errors.Is(err, exit) {
		t.Errorf("error %q does not wrap the exit error", err)
	}
	for _, want := range []string{"go list -deps", "in /repo", "go: cannot find main module"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestBuilderGoListRunnerUsesItsGoSeamAndEnvironment: the builder's guard runs the
// derivation through the builder's own Go seam with its environment, so a test
// that fakes go answers the guard too.
func TestBuilderGoListRunnerUsesItsGoSeamAndEnvironment(t *testing.T) {
	t.Parallel()
	var gotDir string
	var gotEnv, gotArgs []string
	b := Builder{Environ: []string{"A=1"}, Go: func(dir string, env []string, args ...string) ([]byte, error) {
		gotDir, gotEnv, gotArgs = dir, env, args
		return []byte("ok"), nil
	}}

	out, err := b.goListRunner()("/d", "list")

	if err != nil || string(out) != "ok" || gotDir != "/d" || !reflect.DeepEqual(gotEnv, []string{"A=1"}) || !reflect.DeepEqual(gotArgs, []string{"list"}) {
		t.Errorf("runner = %q, %v; Go saw dir %q env %v args %v", out, err, gotDir, gotEnv, gotArgs)
	}
}
