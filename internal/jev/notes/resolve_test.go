package notes

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRepo serves a fixed tree and key set, and counts its reads.
type fakeRepo struct {
	tree     []string
	keys     map[string]bool
	treeErr  error
	treeRead int
	keyRead  int
	synced   int
}

func (f *fakeRepo) Sync(context.Context) error { f.synced++; return nil }

func (f *fakeRepo) Tree(context.Context) ([]string, error) {
	f.treeRead++
	return f.tree, f.treeErr
}

func (f *fakeRepo) HasKey(_ context.Context, key string) (bool, error) {
	f.keyRead++
	return f.keys[key], nil
}

// fakeHelp serves help texts by "bin path..." and counts each run.
type fakeHelp struct {
	texts map[string]string
	runs  map[string]int
}

func (f *fakeHelp) Help(_ context.Context, bin string, path ...string) (string, error) {
	key := strings.TrimSpace(bin + " " + strings.Join(path, " "))
	if f.runs == nil {
		f.runs = map[string]int{}
	}
	f.runs[key]++
	if bin == "absent" {
		return "", ErrNoBinary
	}
	if t, ok := f.texts[key]; ok {
		return t, nil
	}
	return "", errors.New("no help for " + key)
}

const gtRootHelp = `Gas Town (gt) manages multi-agent workspaces.

Usage:
  gt [command]

Work Management:
  bead         File, edit, claim, comment on and close beads
  done         Submit your branch for landing
  hook         Show or attach work on a hook

Services:
  dolt         Manage the Dolt SQL server

Flags:
  -h, --help   help for gt
`

const gtDoneHelp = `Submit your branch for landing.

Examples:
  gt done --issue gt-abc               # Explicit issue ID
  gt hook                              # not a subcommand line

Usage:
  gt done [flags]

Flags:
      --status string   Exit status
      --issue string    Issue ID
  -h, --help            help for done
`

const gtBeadHelp = `Write verbs on beads.

  gt bead create "title"               File a bead

Usage:
  gt bead [flags]
  gt bead [command]

Available Commands:
  close       Close beads
  create      File a bead

Flags:
  -h, --help   help for bead
`

const gtHookHelp = `Show or attach work on a hook.

Usage:
  gt hook [bead-id] [target] [flags]
  gt hook [command]

Available Commands:
  status      Show the hook

Flags:
  -h, --help   help for hook
`

// hmUsage is what hm prints for any --help: one usage line, no per-command help.
const hmUsage = "hm: unknown command \"--help\"\nusage: hm <command> [--json] [flags]\ncommands: context down eval query reindex related status sync up version\n"

func testHelp() *fakeHelp {
	return &fakeHelp{texts: map[string]string{
		"gt":       gtRootHelp,
		"gt done":  gtDoneHelp,
		"gt bead":  gtBeadHelp,
		"gt hook":  gtHookHelp,
		"hm":       hmUsage,
		"hm query": hmUsage,
	}}
}

func testRepo() *fakeRepo {
	return &fakeRepo{
		tree: []string{"README.md", "docs/reference.md", "internal/jev/stale/stale.go", "internal/jev/stale/store.go"},
		keys: map[string]bool{"patrols": true, "jev_stale": true, "enabled": true},
	}
}

func TestResolveLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		text      string
		label     string
		missing   []Token
		unresolve []string
	}{
		{
			name:  "every token resolves",
			text:  "Edit `internal/jev/stale/stale.go` and `docs/reference.md`; run `gt done --status=X` and `gt bead close`.",
			label: LabelCurrent,
		},
		{
			name:    "a path gone from main",
			text:    "See `internal/jev/stale/old.go` and `gt done`.",
			label:   LabelStale,
			missing: []Token{{KindPath, "internal/jev/stale/old.go"}},
		},
		{
			name:    "a directory gone from a conventional root",
			text:    "See `internal/retired/thing.go`.",
			label:   LabelStale,
			missing: []Token{{KindPath, "internal/retired/thing.go"}},
		},
		{
			name:  "a directory with a trailing slash resolves",
			text:  "See `internal/jev/stale/` and `internal/jev`.",
			label: LabelCurrent,
		},
		{
			name:    "a subcommand gone",
			text:    "Run `gt witness status`.",
			label:   LabelStale,
			missing: []Token{{KindCommand, "gt witness"}},
		},
		{
			name:    "a nested subcommand gone",
			text:    "Run `gt bead reopen`.",
			label:   LabelStale,
			missing: []Token{{KindCommand, "gt bead reopen"}},
		},
		{
			name:    "a flag gone",
			text:    "Run `gt done --pre-verified`.",
			label:   LabelStale,
			missing: []Token{{KindFlag, "gt done --pre-verified"}},
		},
		{
			name:  "an argument after a command that takes one is not a subcommand",
			text:  "Run `gt hook gt-abc` and `gt done`.",
			label: LabelCurrent,
		},
		{
			name:  "arguments after a leaf command are not subcommands",
			text:  "Run `gt done gt-abc`.",
			label: LabelCurrent,
		},
		{
			name:  "a binary that prints one usage line is not checked below its root",
			text:  "Run `hm query rig` and `hm related gt-abc`.",
			label: LabelCurrent,
		},
		{
			name:    "a missing hm command is found in its command list",
			text:    "Run `hm frobnicate`.",
			label:   LabelStale,
			missing: []Token{{KindCommand, "hm frobnicate"}},
		},
		{
			name:  "no tokens is unlabeled",
			text:  "Prose about the town with `nothing` checkable.",
			label: "",
		},
		{
			name:      "a config key found nowhere keeps the note from current",
			text:      "Set `patrols.jev_gone.enabled`.",
			label:     "",
			unresolve: []string{"patrols.jev_gone.enabled"},
		},
		{
			name:    "a missing path makes it stale beside an unresolved key",
			text:    "See `internal/jev/stale/old.go` and set `jev_gone`.",
			label:   LabelStale,
			missing: []Token{{KindPath, "internal/jev/stale/old.go"}},
			// The key is unresolved too, and the report keeps both.
			unresolve: []string{"jev_gone"},
		},
		{
			name:  "resolved config keys are current",
			text:  "Set `patrols.jev_stale.enabled`.",
			label: LabelCurrent,
		},
		{
			name:  "paths under a dot directory are not checked",
			text:  "Look in `.beads/metadata.json` and `.runtime/tier-sweep/x.json`.",
			label: "",
		},
		{
			name:  "paths outside the repo's roots are not checked",
			text:  "Look in `crew/sloan/notes.md`.",
			label: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &Resolver{Repo: testRepo(), Help: testHelp()}
			got, err := r.Resolve(context.Background(), Extract(tt.text))
			if err != nil {
				t.Fatal(err)
			}
			if got.Label != tt.label {
				t.Errorf("label = %q, want %q (result %+v)", got.Label, tt.label, got)
			}
			if len(got.Missing) != len(tt.missing) {
				t.Fatalf("missing = %+v, want %+v", got.Missing, tt.missing)
			}
			for i := range tt.missing {
				if got.Missing[i] != tt.missing[i] {
					t.Errorf("missing[%d] = %+v, want %+v", i, got.Missing[i], tt.missing[i])
				}
			}
			var unresolved []string
			for _, u := range got.Unresolved {
				unresolved = append(unresolved, u.Text)
			}
			if strings.Join(unresolved, ",") != strings.Join(tt.unresolve, ",") {
				t.Errorf("unresolved = %v, want %v", unresolved, tt.unresolve)
			}
		})
	}
}

// A command of a binary that is not installed gives no evidence either way:
// it is neither a missing token nor one that resolved.
func TestResolveSkipsAnUninstalledBinary(t *testing.T) {
	t.Parallel()
	r := &Resolver{Repo: testRepo(), Help: testHelp()}
	res := Result{}
	if err := r.resolveCommand(context.Background(), Command{Bin: "absent", Words: []string{"x"}}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Checked != 0 || len(res.Missing) != 0 {
		t.Errorf("result = %+v, want nothing checked", res)
	}
}

// A subcommand the root help names, whose own help cannot be had, gives no
// evidence below it: the walk stops there, the subcommand it matched counts as
// checked, and nothing past it is a missing token. testHelp has no help for gt
// dolt, which the root help lists.
func TestResolveStopsTheWalkWhenASubcommandHasNoHelp(t *testing.T) {
	t.Parallel()
	r := &Resolver{Repo: testRepo(), Help: testHelp()}
	res := Result{}
	if err := r.resolveCommand(context.Background(), Command{Bin: "gt", Words: []string{"dolt", "frobnicate"}}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Checked != 1 || len(res.Missing) != 0 {
		t.Errorf("result = %+v, want the named subcommand checked and nothing missing", res)
	}
}

// An empty tree is no evidence either way: a clone that lists no files says
// nothing about the paths a text names, so a text that names only paths is
// unlabeled rather than stale.
func TestResolveSkipsPathsWhenTheTreeIsEmpty(t *testing.T) {
	t.Parallel()
	r := &Resolver{Repo: &fakeRepo{}, Help: testHelp()}
	got, err := r.Resolve(context.Background(), Extract("See `internal/jev/stale/stale.go` and `docs/reference.md`."))
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "" || got.Checked != 0 || len(got.Missing) != 0 {
		t.Errorf("result = %+v, want nothing checked and no label", got)
	}
}

// One read of the tree, one --help per command and one grep per key segment
// serve every file in a pass.
func TestResolverCachesItsReads(t *testing.T) {
	t.Parallel()
	repo, help := testRepo(), testHelp()
	r := &Resolver{Repo: repo, Help: help}
	text := "`internal/jev/stale/stale.go` `gt done` `gt done --issue=x` `patrols.jev_stale.enabled`"
	for range 3 {
		if _, err := r.Resolve(context.Background(), Extract(text)); err != nil {
			t.Fatal(err)
		}
	}
	if repo.treeRead != 1 {
		t.Errorf("tree read %d times, want 1", repo.treeRead)
	}
	if repo.keyRead != 3 {
		t.Errorf("keys read %d times, want 3 (patrols, jev_stale, enabled)", repo.keyRead)
	}
	if help.runs["gt"] != 1 || help.runs["gt done"] != 1 {
		t.Errorf("help runs = %v, want one each", help.runs)
	}
}

func TestResolveFailsWhenTheTreeCannotBeRead(t *testing.T) {
	t.Parallel()
	repo := testRepo()
	repo.treeErr = errors.New("clone broken")
	r := &Resolver{Repo: repo, Help: testHelp()}
	if _, err := r.Resolve(context.Background(), Extract("`docs/reference.md`")); err == nil {
		t.Fatal("Resolve succeeded with an unreadable tree")
	}
}

func TestHasFlagMatchesWholeFlags(t *testing.T) {
	t.Parallel()
	help := "  --recent-days int   days\n  -r, --recent        recent\n"
	for flag, want := range map[string]bool{"--recent": true, "--recent-days": true, "--rec": false, "--days": false} {
		if got := hasFlag(help, flag); got != want {
			t.Errorf("hasFlag(%q) = %v, want %v", flag, got, want)
		}
	}
}
