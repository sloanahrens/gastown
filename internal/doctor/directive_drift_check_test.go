package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

const driftTestCommit = "0123456789abcdef0123456789abcdef01234567"

// driftCheckWith returns a DirectiveDriftCheck whose build commit and repo
// reader are injected, so no test runs git or reads the host's build.
func driftCheckWith(commit string, repo map[string][]byte, repoErr error) *DirectiveDriftCheck {
	check := NewDirectiveDriftCheck()
	check.buildCommit = func() string { return commit }
	check.readRepo = func(_, _ string) (map[string][]byte, error) {
		return repo, repoErr
	}
	return check
}

func TestDirectiveDriftCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		deployed map[string][]byte
		repo     map[string][]byte
		want     []string
	}{
		{
			name:     "equal sets report nothing",
			deployed: map[string][]byte{"a.md": []byte("A\n")},
			repo:     map[string][]byte{"a.md": []byte("A\n")},
			want:     nil,
		},
		{
			name:     "file missing from the deployed copy",
			deployed: map[string][]byte{},
			repo:     map[string][]byte{"a.md": []byte("A\n")},
			want:     []string{"Missing: directives/a.md is in the repo but not deployed"},
		},
		{
			name:     "extra deployed file",
			deployed: map[string][]byte{"a.md": []byte("A\n")},
			repo:     map[string][]byte{},
			want:     []string{"Extra: directives/a.md is deployed but the repo does not have it"},
		},
		{
			name:     "byte difference",
			deployed: map[string][]byte{"a.md": []byte("hand edit\n")},
			repo:     map[string][]byte{"a.md": []byte("A\n")},
			want:     []string{"Different: directives/a.md differs from the repo's copy"},
		},
		{
			name:     "findings sort by file name",
			deployed: map[string][]byte{"z.md": []byte("Z\n"), "m.md": []byte("old\n")},
			repo:     map[string][]byte{"a.md": []byte("A\n"), "m.md": []byte("M\n")},
			want: []string{
				"Missing: directives/a.md is in the repo but not deployed",
				"Different: directives/m.md differs from the repo's copy",
				"Extra: directives/z.md is deployed but the repo does not have it",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compareDirectives(tt.deployed, tt.repo)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("compareDirectives() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDirectiveDriftCheck_Metadata(t *testing.T) {
	t.Parallel()
	check := NewDirectiveDriftCheck()

	if check.Name() != "directive-drift" {
		t.Errorf("Name() = %q, want %q", check.Name(), "directive-drift")
	}
	if check.Category() != CategoryConfig {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryConfig)
	}
	if !check.CanFix() {
		t.Error("CanFix() = false; redeploying from the repo is the fix")
	}
}

func TestDirectiveDriftCheck_CleanWhenDeployedMatchesRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirectiveFile(t, filepath.Join(town, "directives", "a.md"), "A\n")
	repo := map[string][]byte{"a.md": []byte("A\n")}

	result := driftCheckWith(driftTestCommit, repo, nil).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK: %s %v", result.Status, result.Message, result.Details)
	}
}

func TestDirectiveDriftCheck_OneFindingAfterHandEdit(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirectiveFile(t, filepath.Join(town, "directives", "a.md"), "A\n")
	writeDirectiveFile(t, filepath.Join(town, "directives", "b.md"), "B edited by hand\n")
	repo := map[string][]byte{"a.md": []byte("A\n"), "b.md": []byte("B\n")}

	result := driftCheckWith(driftTestCommit, repo, nil).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning: %s", result.Status, result.Message)
	}
	want := []string{"Different: directives/b.md differs from the repo's copy"}
	if !reflect.DeepEqual(result.Details, want) {
		t.Errorf("Details = %q, want %q", result.Details, want)
	}
}

func TestDirectiveDriftCheck_MissingDeployedDirReportsEveryRepoFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := map[string][]byte{"a.md": []byte("A\n")}

	result := driftCheckWith(driftTestCommit, repo, nil).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning: %s", result.Status, result.Message)
	}
	want := []string{"Missing: directives/a.md is in the repo but not deployed"}
	if !reflect.DeepEqual(result.Details, want) {
		t.Errorf("Details = %q, want %q", result.Details, want)
	}
}

func TestDirectiveDriftCheck_CommitMissingFromRepoWarnsNamingCommit(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	readErr := fmt.Errorf("central directives: commit %s is not in x: %w", driftTestCommit, config.ErrDirectiveCommitMissing)

	result := driftCheckWith(driftTestCommit, nil, readErr).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning, not an error: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, driftTestCommit) {
		t.Errorf("Message = %q, want it to name commit %s", result.Message, driftTestCommit)
	}
}

func TestDirectiveDriftCheck_UnreadableRepoIsSkippedNotClean(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirectiveFile(t, filepath.Join(town, "directives", "a.md"), "A\n")

	result := driftCheckWith(driftTestCommit, nil, fmt.Errorf("git exploded")).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want Skipped: an unread repo must not pass as clean", result.Status)
	}
}

func TestDirectiveDriftCheck_NoBuildCommitIsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	check := driftCheckWith("", nil, nil)
	check.readRepo = func(_, _ string) (map[string][]byte, error) {
		t.Error("readRepo called with no build commit to name")
		return nil, nil
	}

	result := check.Run(&CheckContext{TownRoot: town})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want Skipped", result.Status)
	}
}

func TestDirectiveDriftCheck_SymlinkedDeployedFileIsError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := filepath.Join(town, "directives")
	writeDirectiveFile(t, filepath.Join(town, "elsewhere.md"), "A\n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(town, "elsewhere.md"), filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	repo := map[string][]byte{"a.md": []byte("A\n")}

	result := driftCheckWith(driftTestCommit, repo, nil).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusError {
		t.Fatalf("Status = %v, want Error: %s", result.Status, result.Message)
	}
	want := []string{"Not a regular file: directives/a.md"}
	if !reflect.DeepEqual(result.Details, want) {
		t.Errorf("Details = %q, want %q", result.Details, want)
	}
}

func TestDirectiveDriftCheck_UnreadableDeployedDirIsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirectiveFile(t, filepath.Join(town, "directives"), "a file where the directory belongs")

	result := driftCheckWith(driftTestCommit, map[string][]byte{"a.md": []byte("A\n")}, nil).Run(&CheckContext{TownRoot: town})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want Skipped: a deployed dir that cannot be listed must not pass as clean", result.Status)
	}
}

func TestDirectiveDriftCheck_UnreadableDeployedFileIsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDirectiveFile(t, filepath.Join(town, "directives", "a.md"), "A\n")
	check := driftCheckWith(driftTestCommit, map[string][]byte{"a.md": []byte("A\n")}, nil)
	check.readFile = func(string) ([]byte, error) { return nil, fmt.Errorf("read refused") }

	result := check.Run(&CheckContext{TownRoot: town})

	if result.Status != StatusSkipped {
		t.Errorf("Status = %v, want Skipped: an unread deployed file must not pass as clean", result.Status)
	}
}

func TestDirectiveDriftCheck_FixWithNoCommitRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	check := driftCheckWith("", nil, nil)

	if err := check.Fix(&CheckContext{TownRoot: town}); err == nil {
		t.Fatal("Fix() = nil, want an error: the build has no commit to deploy from")
	}
	if _, err := os.Stat(config.CentralDirectiveDir(town)); !os.IsNotExist(err) {
		t.Errorf("Fix with no commit created %s (stat err %v); a refused fix writes nothing", config.CentralDirectiveDir(town), err)
	}
}
