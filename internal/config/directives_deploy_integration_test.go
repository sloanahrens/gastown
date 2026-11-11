//go:build integration

package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/directivegit"
)

// The deploy reads a real gastown bare repo: files written at the commit, a
// file the commit dropped removed, a second run a no-op, an unknown commit
// refused with nothing written, an empty commit resolved to the build commit,
// and a committed symlink refused by name.
func TestIntegrationDeployCentralDirectivesFromTheBareRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := directivegit.NewBare(t, CentralDirectiveRepo(town))
	first := directivegit.Commit(t, repo, "", map[string]directivegit.Entry{
		"a.md":     {Content: "A1\n"},
		"stale.md": {Content: "gone\n"},
	})
	second := directivegit.Commit(t, repo, first, map[string]directivegit.Entry{
		"a.md": {Content: "A2\n"},
	})

	written, err := DeployCentralDirectives(town, first, nil)
	if err != nil {
		t.Fatalf("deploy first commit: %v", err)
	}
	if got := baseNames(written); !reflect.DeepEqual(got, []string{"a.md", "stale.md"}) {
		t.Errorf("first deploy wrote %v, want a.md and stale.md", got)
	}

	written, err = DeployCentralDirectives(town, second, nil)
	if err != nil {
		t.Fatalf("deploy second commit: %v", err)
	}
	if got := baseNames(written); !reflect.DeepEqual(got, []string{"a.md"}) {
		t.Errorf("second deploy wrote %v, want a.md alone", got)
	}
	want := map[string]string{"a.md": "A2\n"}
	if got := dirContents(t, CentralDirectiveDir(town)); !reflect.DeepEqual(got, want) {
		t.Errorf("deployed after second commit = %v, want %v (stale.md removed)", got, want)
	}

	written, err = DeployCentralDirectives(town, second, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("second run wrote %v, want nothing", baseNames(written))
	}

	const unknown = "1234567890abcdef1234567890abcdef12345678"
	if _, err := DeployCentralDirectives(town, unknown, nil); err == nil || !strings.Contains(err.Error(), unknown) {
		t.Errorf("unknown commit error = %v, want one naming %s", err, unknown)
	}
	if got := dirContents(t, CentralDirectiveDir(town)); !reflect.DeepEqual(got, want) {
		t.Errorf("unknown commit changed the deployed directives to %v", got)
	}

	if _, err := DeployCentralDirectives(town, "", func() string { return second }); err != nil {
		t.Errorf("empty commit with build commit %s: %v", second, err)
	}

	withLink := directivegit.Commit(t, repo, second, map[string]directivegit.Entry{
		"a.md":    {Content: "A3\n"},
		"link.md": {Content: "../../etc/passwd", Symlink: true},
	})
	if _, err := DeployCentralDirectives(town, withLink, nil); err == nil || !strings.Contains(err.Error(), "link.md") {
		t.Errorf("symlink commit error = %v, want one naming link.md", err)
	}
	if got := dirContents(t, CentralDirectiveDir(town)); !reflect.DeepEqual(got, want) {
		t.Errorf("symlink commit changed the deployed directives to %v", got)
	}
}
