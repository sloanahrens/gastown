package rehearsal

import (
	"flag"
	"fmt"
	"io"
)

// RunArgs is the parsed command line of `gt-rehearsal run`.
type RunArgs struct {
	Rig           string
	Set           string
	Worktrees     [3]string
	Agent         string
	ChangeSet     string
	Tag           string
	CacheBytes    int64
	Interventions []string
	Record        string
}

// ParseRunArgs parses the flags of `gt-rehearsal run` and refuses a missing
// required flag, a negative --cache-bytes or an empty --rig, naming the flag.
func ParseRunArgs(args []string) (RunArgs, error) {
	fs := flag.NewFlagSet("gt-rehearsal run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var a RunArgs
	fs.StringVar(&a.Rig, "rig", "rehearsal", "rig the run files its beads in")
	fs.StringVar(&a.Set, "set", "", "directory of the sixteen bead definitions")
	fs.StringVar(&a.Worktrees[0], "worktree-1", "", "first of three side-load worktrees")
	fs.StringVar(&a.Worktrees[1], "worktree-2", "", "second of three side-load worktrees")
	fs.StringVar(&a.Worktrees[2], "worktree-3", "", "third of three side-load worktrees")
	fs.StringVar(&a.Agent, "agent", "", "seat agent the run expects")
	fs.StringVar(&a.ChangeSet, "change-set", "", "the one change set the run names")
	fs.StringVar(&a.Tag, "tag", "", "tagged commit the worktrees are at")
	fs.Int64Var(&a.CacheBytes, "cache-bytes", -1, "shared sandbox cache size in bytes")
	fs.Func("intervention", "an intervention the run records; repeat for each", func(s string) error {
		a.Interventions = append(a.Interventions, s)
		return nil
	})
	fs.StringVar(&a.Record, "record", "", "path of the run record JSON file")
	if err := fs.Parse(args); err != nil {
		return RunArgs{}, fmt.Errorf("gt-rehearsal run: %w", err)
	}
	for _, f := range []struct{ name, value string }{
		{"--set", a.Set}, {"--worktree-1", a.Worktrees[0]}, {"--worktree-2", a.Worktrees[1]},
		{"--worktree-3", a.Worktrees[2]}, {"--agent", a.Agent}, {"--change-set", a.ChangeSet},
		{"--tag", a.Tag}, {"--record", a.Record},
	} {
		if f.value == "" {
			return RunArgs{}, fmt.Errorf("gt-rehearsal run: %s is required", f.name)
		}
	}
	if a.CacheBytes < 0 {
		return RunArgs{}, fmt.Errorf("gt-rehearsal run: --cache-bytes is required and must not be negative")
	}
	if a.Rig == "" {
		return RunArgs{}, fmt.Errorf("gt-rehearsal run: --rig must not be empty")
	}
	return a, nil
}
