package daemon

import (
	"log"
	"path/filepath"
	"strings"
	"testing"
)

// The push refusal for a backup repo with no origin names the repo path and
// says an origin must be added to that repo, and carries no URL.
func TestJSONLGitBackupPushRefusalNamesRepoPathWithoutURL(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	repo := t.TempDir()
	initEmptyGitRepo(t, d, repo)

	err := d.pushJsonlBackup(repo, true, "msg")
	if err == nil {
		t.Fatal("pushJsonlBackup to a repo with no origin succeeded, want a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, repo) {
		t.Errorf("refusal %q does not name the repo path %s", msg, repo)
	}
	if !strings.Contains(msg, "an origin must be added to that repo") {
		t.Errorf("refusal %q does not say an origin must be added to the repo", msg)
	}
	if strings.Contains(msg, "://") {
		t.Errorf("refusal %q carries a URL", msg)
	}
}

// The resolved repo path is logged once per process, with its source, and two
// cycles on the same daemon write that line once.
func TestJSONLGitBackupLogsResolvedRepoOncePerProcess(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	setRepo := filepath.Join(t.TempDir(), "offsite")
	for _, tc := range []struct {
		name    string
		gitRepo string
		repo    string
		source  string
	}{
		{name: "default", gitRepo: "", repo: filepath.Join(townRoot, ".dolt-archive", "git"), source: "default"},
		{name: "setting", gitRepo: setRepo, repo: setRepo, source: "set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := backupTestDaemon(t)
			var logs strings.Builder
			d.logger = log.New(&logs, "", 0)
			d.config = &Config{TownRoot: townRoot}
			d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
				JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: tc.gitRepo},
			}}

			d.syncJsonlGitBackup()
			d.syncJsonlGitBackup()

			want := "jsonl_git_backup: repo " + tc.repo + " (git_repo setting " + tc.source + ")"
			var got []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.HasPrefix(line, "jsonl_git_backup: repo ") {
					got = append(got, line)
				}
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("resolved-repo log lines = %q over two cycles, want exactly [%q]", got, want)
			}
		})
	}
}

// JsonlBackupRepo is the one resolver the patrol and the doctor check share: a
// configured git_repo wins, and the default is under the town root.
func TestJsonlBackupRepoResolvesSettingOrDefault(t *testing.T) {
	t.Parallel()
	town := "/town"
	setRepo := "/elsewhere/offsite"
	for _, tc := range []struct {
		name       string
		cfg        *DaemonPatrolConfig
		wantPath   string
		wantSource bool
	}{
		{name: "no config", cfg: nil, wantPath: "/town/.dolt-archive/git", wantSource: false},
		{name: "no patrols", cfg: &DaemonPatrolConfig{}, wantPath: "/town/.dolt-archive/git", wantSource: false},
		{name: "no jsonl block", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, wantPath: "/town/.dolt-archive/git", wantSource: false},
		{name: "empty git_repo", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JsonlGitBackup: &JsonlGitBackupConfig{}}}, wantPath: "/town/.dolt-archive/git", wantSource: false},
		{name: "git_repo set", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JsonlGitBackup: &JsonlGitBackupConfig{GitRepo: setRepo}}}, wantPath: setRepo, wantSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, fromSetting := JsonlBackupRepo(town, tc.cfg)
			if path != tc.wantPath || fromSetting != tc.wantSource {
				t.Errorf("JsonlBackupRepo = (%q, %v), want (%q, %v)", path, fromSetting, tc.wantPath, tc.wantSource)
			}
		})
	}
}
