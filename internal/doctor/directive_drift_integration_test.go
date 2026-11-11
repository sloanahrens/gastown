//go:build integration

package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/testutil/directivegit"
)

// The drift check reads a real gastown bare repo and is compared with what
// DeployCentralDirectives wrote from it: clean after a deploy, one finding
// after a hand edit, a warning for a commit the repo lacks, and Fix restores
// the deployed copy. Real git, so it carries the integration tag; the gate
// does not run it.
func TestIntegrationDirectiveDriftFromTheBareRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := directivegit.NewBare(t, config.CentralDirectiveRepo(town))
	commit := directivegit.Commit(t, repo, "", map[string]directivegit.Entry{
		"a.md": {Content: "A\n"},
		"b.md": {Content: "B\n"},
	})
	if _, err := config.DeployCentralDirectives(town, commit, nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	check := NewDirectiveDriftCheck()
	check.buildCommit = func() string { return commit }
	ctx := &CheckContext{TownRoot: town}

	if r := check.Run(ctx); r.Status != StatusOK {
		t.Fatalf("after deploy: Status = %v, want OK: %s %v", r.Status, r.Message, r.Details)
	}

	handEdited := filepath.Join(config.CentralDirectiveDir(town), "b.md")
	if err := os.WriteFile(handEdited, []byte("B edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := check.Run(ctx)
	if r.Status != StatusWarning {
		t.Fatalf("after hand edit: Status = %v, want Warning: %s", r.Status, r.Message)
	}
	if want := []string{"Different: directives/b.md differs from the repo's copy"}; !reflect.DeepEqual(r.Details, want) {
		t.Errorf("after hand edit: Details = %q, want %q", r.Details, want)
	}

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("fix: %v", err)
	}
	if r := check.Run(ctx); r.Status != StatusOK {
		t.Errorf("after fix: Status = %v, want OK: %s %v", r.Status, r.Message, r.Details)
	}

	const unknown = "1234567890abcdef1234567890abcdef12345678"
	check.buildCommit = func() string { return unknown }
	r = check.Run(ctx)
	if r.Status != StatusWarning {
		t.Fatalf("commit missing from repo: Status = %v, want Warning: %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, unknown) {
		t.Errorf("commit missing from repo: Message = %q, want it to name %s", r.Message, unknown)
	}
}
