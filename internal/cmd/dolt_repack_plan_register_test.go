package cmd

import (
	"bytes"
	"errors"
	"reflect"
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

// TestRunDoltRepackPlanCmd: the configured git_repo wins over the default, and
// each unreadable input is an error that prints nothing and runs no git.
func TestRunDoltRepackPlanCmd(t *testing.T) {
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
		var calls [][]string
		run := func(argv ...string) ([]byte, error) {
			calls = append(calls, argv)
			return []byte("count: 1\nsize: 1\n"), nil
		}
		var out bytes.Buffer
		err := runDoltRepackPlanCmd(&out, tc.town, tc.read, run)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out.Len() != 0 || len(calls) != 0 {
				t.Errorf("%s: got (%q, %v, %q), want an error naming %q and nothing run or printed", tc.name, out.String(), err, calls, tc.wantErr)
			}
			continue
		}
		want := [][]string{{"git", "-C", tc.repo, "count-objects", "-v"}}
		if err != nil || !strings.Contains(out.String(), "due: no") || !reflect.DeepEqual(calls, want) {
			t.Errorf("%s: got (%q, %v, %q), want the plan from count-objects in %q", tc.name, out.String(), err, calls, tc.repo)
		}
	}
}
