package rehearsal

import (
	"reflect"
	"strings"
	"testing"
)

// runArgsBase is one complete command line; each refusal row changes one flag.
var runArgsBase = []string{
	"--set", "/set", "--worktree-1", "/w1", "--worktree-2", "/w2", "--worktree-3", "/w3", "--agent", "deepseek",
	"--change-set", "cs-1", "--tag", "v1", "--cache-bytes", "4096", "--record", "/run.json",
}

func TestRehearsalRunArgsParses(t *testing.T) {
	t.Parallel()
	args := append(runArgsBase, "--intervention", "held dispatch", "--intervention", "raised cap")
	got, err := ParseRunArgs(args)
	if err != nil {
		t.Fatalf("ParseRunArgs: %v", err)
	}
	want := RunArgs{Rig: "rehearsal", Set: "/set", Worktrees: [3]string{"/w1", "/w2", "/w3"}, Agent: "deepseek",
		ChangeSet: "cs-1", Tag: "v1", CacheBytes: 4096, Interventions: []string{"held dispatch", "raised cap"}, Record: "/run.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRunArgs = %+v, want %+v", got, want)
	}
}

func TestRehearsalRunArgsMissing(t *testing.T) {
	t.Parallel()
	for _, drop := range []string{"--set", "--worktree-1", "--worktree-2", "--worktree-3", "--agent", "--change-set", "--tag", "--cache-bytes", "--record"} {
		t.Run(drop, func(t *testing.T) {
			t.Parallel()
			var args []string
			for i := 0; i < len(runArgsBase); i += 2 {
				if runArgsBase[i] != drop {
					args = append(args, runArgsBase[i], runArgsBase[i+1])
				}
			}
			if _, err := ParseRunArgs(args); err == nil || !strings.Contains(err.Error(), drop) {
				t.Errorf("ParseRunArgs without %s: err = %v, want an error naming it", drop, err)
			}
		})
	}
}

func TestRehearsalRunArgsValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, flag, want string }{
		{"negative cache bytes", "--cache-bytes=-5", "--cache-bytes"}, {"cache bytes not a number", "--cache-bytes=many", "cache-bytes"}, {"empty rig", "--rig=", "--rig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRunArgs(append(runArgsBase, tc.flag)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseRunArgs with %s: err = %v, want an error naming %q", tc.flag, err, tc.want)
			}
		})
	}
}
