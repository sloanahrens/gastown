package rehearsal

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRehearsalRunOptionsGoodInputs(t *testing.T) {
	t.Parallel()
	root := runInputsRoot(t)
	a := runInputsArgs(root)
	a.Agent, a.ChangeSet, a.Tag = "deepseek", "cs-1", "v1"
	a.CacheBytes, a.Interventions, a.Record = 4096, []string{"held dispatch"}, "run.json"
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.FixedZone("run zone", 5*60*60))
	o, err := NewRunOptions(a, root, func() time.Time { return at })
	cache, trees := filepath.Join(root, "run.json.gocache"), []string{filepath.Join(root, "w1"), filepath.Join(root, "w2"), filepath.Join(root, "w3")}
	if err != nil || o.Town.Rig != "rehearsal" || o.Town.Run == nil || o.Agent != "deepseek" || !slices.Equal(o.Worktrees, trees) || o.CacheRoot != cache ||
		o.SideRun == nil || o.Now == nil || !o.Now().Equal(at) || o.Tick == nil || o.Deadline == nil || o.Deadline(0) == nil || o.RunID != "rehearsal-20261127T050000Z" ||
		o.Tag != "v1" || o.ChangeSet != "cs-1" || o.CacheBytes != 4096 || !slices.Equal(o.Interventions, a.Interventions) || len(o.Tasks) != 16 || o.Path != filepath.Join(root, "run.json") {
		t.Fatalf("NewRunOptions = %+v, err %v; want the run's Options", o, err)
	}
	if load, err := io.ReadAll(o.HostLoad); err != nil || string(load) != "2026-11-27T10:00:00Z load1=1.00 load5=1.00 load15=1.00 ncpu=24\n" {
		t.Errorf("HostLoad = %q, err %v; want today's host load file", load, err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatalf("cache root %s holds %d entries, err %v; want an empty directory", cache, len(entries), err)
	}
}

func TestRehearsalRunOptionsRefusesAndCreatesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, root string)
		edit   func(a *RunArgs)
		want   string
		exists bool
	}{
		{"refused input", nil, func(a *RunArgs) { a.Rig = "gone" }, "gone", false},
		{"existing cache root", func(t *testing.T, root string) { must(t, os.Mkdir(filepath.Join(root, "run.json.gocache"), 0o700)) }, nil, "run.json.gocache", true},
		{"missing record parent", nil, func(a *RunArgs) { a.Record = "absent/run.json" }, filepath.Join("absent", "run.json.gocache"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := runInputsRoot(t)
			if tc.setup != nil {
				tc.setup(t, root)
			}
			a := runInputsArgs(root)
			a.Record = "run.json"
			if tc.edit != nil {
				tc.edit(&a)
			}
			if _, err := NewRunOptions(a, root, func() time.Time { return runInputsDay }); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewRunOptions: err = %v, want an error naming %s", err, tc.want)
			}
			cache := filepath.Join(root, a.Record+".gocache")
			if entries, err := os.ReadDir(cache); tc.exists && (err != nil || len(entries) != 0) || !tc.exists && !os.IsNotExist(err) {
				t.Errorf("cache root %s holds %d entries, err %v; want exists=%v and empty", cache, len(entries), err, tc.exists)
			}
		})
	}
}
