package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
)

// TestDoltRepackPlanIsRegistered: gt dolt offers repack-plan, which takes no
// argument. Without the AddCommand line in dolt.go's init, Find fails.
func TestDoltRepackPlanIsRegistered(t *testing.T) {
	t.Parallel()
	found, _, err := doltCmd.Find([]string{"repack-plan"})
	if err != nil || found != doltRepackPlanCmd {
		t.Fatalf("gt dolt does not offer repack-plan: found %q, err %v", found.Name(), err)
	}
	noArg := found.Args(found, nil) == nil
	takesArg := found.Args(found, []string{"extra"}) == nil
	if !noArg || takesArg {
		t.Errorf("repack-plan: accepts no argument = %v, accepts an extra one = %v", noArg, takesArg)
	}
}

// TestDoltArchiveRepo: the configured git_repo wins, and each unreadable
// input is an error naming which one failed.
func TestDoltArchiveRepo(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	town := func() (string, error) { return "/town", nil }
	cases := []struct {
		name          string
		town          func() (string, error)
		read          func(string) (*daemon.DaemonPatrolConfig, error)
		repo, wantErr string
	}{
		{"no town", func() (string, error) { return "", refused }, nil, "", "not in a Gas Town workspace"},
		{"no config", town, func(string) (*daemon.DaemonPatrolConfig, error) { return nil, refused }, "", "reading the daemon config"},
		{"default", town, func(string) (*daemon.DaemonPatrolConfig, error) { return nil, nil }, "/town/.dolt-archive/git", ""},
		{"configured", town, func(string) (*daemon.DaemonPatrolConfig, error) {
			return &daemon.DaemonPatrolConfig{Patrols: &daemon.PatrolsConfig{
				JsonlGitBackup: &daemon.JsonlGitBackupConfig{GitRepo: "/backup/git"}}}, nil
		}, "/backup/git", ""},
	}
	for _, tc := range cases {
		got, err := doltArchiveRepo(tc.town, tc.read)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Errorf("%s: got (%q, %v), want an error naming %q", tc.name, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.repo {
			t.Errorf("%s: got (%q, %v), want (%q, nil)", tc.name, got, err, tc.repo)
		}
	}
}
