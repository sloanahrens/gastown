package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// writePatrolConfig writes the town's daemon patrol config (mayor/daemon.json),
// the file the patrol and the doctor both read.
func writePatrolConfig(t *testing.T, town, body string) {
	t.Helper()
	path := daemon.PatrolConfigFile(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backupRepoAt makes an offsite backup repo at dir in f: a repository with no
// remotes, and the .git directory the patrol's own existence check looks for.
func backupRepoAt(t *testing.T, f *gitfake.Fake, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.InitRepo(t, dir)
}

// remotesFails is a repository whose remote listing fails: the check cannot say
// whether origin exists, so it must not report one missing.
type remotesFails struct{ Repo }

func (remotesFails) Remotes() ([]string, error) {
	return nil, errors.New("git remote: exit status 128")
}

func TestJsonlBackupOriginCheck_NoOriginReportsPathWhenDefault(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := filepath.Join(town, ".dolt-archive", "git")
	f := gitfake.New()
	backupRepoAt(t, f, repo)

	result := NewJsonlBackupOriginCheck().Run(withGit(&CheckContext{TownRoot: town}, f))

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning for a repo with no origin: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, repo) {
		t.Errorf("message %q does not name the default repo %s", result.Message, repo)
	}
}

func TestJsonlBackupOriginCheck_NoOriginReportsConfiguredPath(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := filepath.Join(t.TempDir(), "offsite")
	writePatrolConfig(t, town, `{"type":"daemon-patrol-config","version":1,"patrols":{"jsonl_git_backup":{"enabled":true,"git_repo":"`+repo+`"}}}`)
	f := gitfake.New()
	backupRepoAt(t, f, repo)

	result := NewJsonlBackupOriginCheck().Run(withGit(&CheckContext{TownRoot: town}, f))

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning for a configured repo with no origin: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, repo) {
		t.Errorf("message %q does not name the configured repo %s", result.Message, repo)
	}
	if defaultRepo := filepath.Join(town, ".dolt-archive", "git"); strings.Contains(result.Message, defaultRepo) {
		t.Errorf("message %q names the default repo %s, not the configured one", result.Message, defaultRepo)
	}
}

func TestJsonlBackupOriginCheck_OriginPresentIsOK(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := filepath.Join(town, ".dolt-archive", "git")
	f := gitfake.New()
	backupRepoAt(t, f, repo)
	fetchRemote(t, f, repo, "origin", fakeRemote(t, f, filepath.Join(town, "offsite.git"), nil))

	result := NewJsonlBackupOriginCheck().Run(withGit(&CheckContext{TownRoot: town}, f))

	if result.Status != StatusOK {
		t.Errorf("status = %v, want StatusOK for a repo with an origin: %s", result.Status, result.Message)
	}
}

func TestJsonlBackupOriginCheck_AbsentRepoIsNotAFinding(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := gitfake.New()

	result := NewJsonlBackupOriginCheck().Run(withGit(&CheckContext{TownRoot: town}, f))

	if result.Status != StatusOK {
		t.Errorf("status = %v, want StatusOK for an absent repo (the patrol creates it): %s", result.Status, result.Message)
	}
}

func TestJsonlBackupOriginCheck_RemoteListFailureIsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo := filepath.Join(town, ".dolt-archive", "git")
	f := gitfake.New()
	backupRepoAt(t, f, repo)
	ctx := &CheckContext{TownRoot: town}
	ctx.openGit = func(gitDir, workDir string) Repo {
		return remotesFails{f.OpenWithDir(gitDir, workDir).(Repo)}
	}

	result := NewJsonlBackupOriginCheck().Run(ctx)

	if result.Status != StatusSkipped {
		t.Errorf("status = %v, want StatusSkipped when the remote listing fails: %s", result.Status, result.Message)
	}
}

func TestJsonlBackupOriginCheck_UnreadablePatrolConfigIsSkipped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writePatrolConfig(t, town, `{"type":"daemon-patrol-config","version":`)
	f := gitfake.New()

	result := NewJsonlBackupOriginCheck().Run(withGit(&CheckContext{TownRoot: town}, f))

	if result.Status != StatusSkipped {
		t.Errorf("status = %v, want StatusSkipped for an unreadable patrol config: %s", result.Status, result.Message)
	}
}
