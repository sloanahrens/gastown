package bird

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// tty is the Terminal of a caller at a shell.
var tty = Terminal{Stdin: true, Stdout: true}

// fakeLookup is an in-memory registry for Plan's tests: no file, no lock and
// no process table.
type fakeLookup struct {
	names    []string
	bound    map[string]string
	taken    map[string]bool
	boundErr error
	ownsErr  error
	takenErr error
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{
		names: []string{"wren", "robin", "lark"},
		bound: map[string]string{},
		taken: map[string]bool{},
	}
}

// bindFake returns an in-memory registry that has name bound to bead.
func bindFake(bead, name string) *fakeLookup {
	l := newFakeLookup()
	l.bound[bead] = name
	return l
}

func (f *fakeLookup) Bound(bead string) (string, bool, error) {
	if f.boundErr != nil {
		return "", false, f.boundErr
	}
	name, ok := f.bound[bead]
	return name, ok, nil
}

func (f *fakeLookup) Owns(name string) (bool, error) {
	if f.ownsErr != nil {
		return false, f.ownsErr
	}
	if slices.Contains(f.names, name) {
		return true, nil
	}
	for _, bound := range f.bound {
		if bound == name {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeLookup) Taken() (map[string]bool, error) {
	if f.takenErr != nil {
		return nil, f.takenErr
	}
	out := make(map[string]bool, len(f.taken))
	for name, held := range f.taken {
		out[name] = held
	}
	return out, nil
}

func (f *fakeLookup) Names() []string { return slices.Clone(f.names) }

// launched fails the test unless Plan chose a launch.
func launched(t *testing.T, d Decision) *Launch {
	t.Helper()
	if d.Launch == nil {
		t.Fatalf("Plan passed claude through, want a launch (stderr %q)", d.Stderr)
	}
	return d.Launch
}

// passedThrough fails the test unless Plan left claude to the caller.
func passedThrough(t *testing.T, d Decision) {
	t.Helper()
	if d.Launch != nil {
		t.Fatalf("Plan launched claude as %q, want a passthrough", d.Launch.Argv)
	}
}

// envValue returns the one value env gives key, failing unless it defines it
// exactly once.
func envValue(t *testing.T, env []string, key string) string {
	t.Helper()
	var values []string
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			values = append(values, v)
		}
	}
	if len(values) != 1 {
		t.Fatalf("env defines %s %d times, want once: %q", key, len(values), env)
	}
	return values[0]
}

// checkIdentity asserts launch carries name's whole identity and no one else's.
func checkIdentity(t *testing.T, launch *Launch, name string) {
	t.Helper()
	want := map[string]string{
		"CLAUDE_AGENT_NAME":   name,
		"BEADS_ACTOR":         "town/crew/" + name,
		"BD_ACTOR":            "town/crew/" + name,
		"GIT_AUTHOR_NAME":     name,
		"GIT_COMMITTER_NAME":  name,
		"GIT_AUTHOR_EMAIL":    name + "@gastown.local",
		"GIT_COMMITTER_EMAIL": name + "@gastown.local",
	}
	for key, value := range want {
		if got := envValue(t, launch.Env, key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if want := []string{"--name", name}; len(launch.Argv) < 2 || !slices.Equal(launch.Argv[:2], want) {
		t.Errorf("argv starts %q, want it to start %q", launch.Argv, want)
	}
}

func TestPlanPassesThroughWithAnIdentityAlreadySet(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_ACTOR", "CLAUDE_AGENT_NAME"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(nil, []string{key + "=gastown/polecats/capable"}, tty, newFakeLookup())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			passedThrough(t, d)
		})
	}
}

func TestPlanLaunchesWithOnlyAGitIdentitySet(t *testing.T) {
	t.Parallel()
	d, err := Plan(nil, []string{"GIT_AUTHOR_NAME=someone"}, tty, newFakeLookup())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	checkIdentity(t, launched(t, d), "wren")
}

func TestPlanPassesThroughWhenAStreamIsNotATerminal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		term Terminal
	}{
		{"stdin", Terminal{Stdin: false, Stdout: true}},
		{"stdout", Terminal{Stdin: true, Stdout: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(nil, nil, tc.term, newFakeLookup())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			passedThrough(t, d)
		})
	}
}

func TestPlanPassesThroughForASelfFlag(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-p", "--print", "-h", "--help", "-v", "--version", "-c", "--continue"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			d, err := Plan([]string{flag}, nil, tty, newFakeLookup())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			passedThrough(t, d)
		})
	}
}

func TestPlanPassesThroughForASubcommandWord(t *testing.T) {
	t.Parallel()
	d, err := Plan([]string{"mcp"}, nil, tty, newFakeLookup())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	passedThrough(t, d)
}

func TestPlanLaunchesForAPromptWithSpaces(t *testing.T) {
	t.Parallel()
	d, err := Plan([]string{"fix the failing test"}, nil, tty, newFakeLookup())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	launch := launched(t, d)
	checkIdentity(t, launch, "wren")
	if want := []string{"--name", "wren", "fix the failing test"}; !slices.Equal(launch.Argv, want) {
		t.Errorf("argv = %q, want %q", launch.Argv, want)
	}
}

func TestPlanKeepsAnExplicitName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"separate value", []string{"--name", "robin", "fix it"}},
		{"inline value", []string{"--name=robin", "fix it"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(tc.args, nil, tty, bindFake("gt-1", "lark"))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			launch := launched(t, d)
			if launch.Name != "robin" {
				t.Errorf("name = %q, want robin", launch.Name)
			}
			checkIdentity(t, launch, "robin")
			if want := []string{"--name", "robin", "fix it"}; !slices.Equal(launch.Argv, want) {
				t.Errorf("argv = %q, want %q", launch.Argv, want)
			}
		})
	}
}

// TestPlanMarksOnlyACallerChosenNamePinned: a name the caller named itself, or
// resumed, may take over the reservation a spawn made for it; a name Plan draws
// from the pool may not, since no spawn reserved it (gt-b58mk).
func TestPlanMarksOnlyACallerChosenNamePinned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{name: "a name the caller gave", args: []string{"--name", "robin"}, want: true},
		{name: "a bird of ours resumed", args: []string{"--resume", "lark"}, want: true},
		{name: "a name drawn from the pool", args: []string{"fix it"}, want: false},
		{name: "the name a bead is bound to", args: []string{"--bead", "gt-1", "fix it"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(tc.args, nil, tty, bindFake("gt-1", "lark"))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if got := launched(t, d).Pinned; got != tc.want {
				t.Errorf("Pinned = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanPassesThroughForANameThatIsNotAName(t *testing.T) {
	t.Parallel()
	d, err := Plan([]string{"--name", "not a name"}, nil, tty, newFakeLookup())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	passedThrough(t, d)
}

func TestPlanReusesTheNameBoundToABead(t *testing.T) {
	t.Parallel()
	d, err := Plan([]string{"--bead", "gt-1.a_b", "--verbose", "fix it"}, nil, tty, bindFake("gt-1.a_b", "lark"))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	launch := launched(t, d)
	if launch.Name != "lark" {
		t.Errorf("name = %q, want lark", launch.Name)
	}
	checkIdentity(t, launch, "lark")
	if want := []string{"--name", "lark", "--verbose", "fix it"}; !slices.Equal(launch.Argv, want) {
		t.Errorf("argv = %q, want %q", launch.Argv, want)
	}
}

func TestPlanPicksAFreeNameForANewBead(t *testing.T) {
	t.Parallel()
	lookup := newFakeLookup()
	lookup.taken["wren"] = true
	d, err := Plan([]string{"--bead", "gt-2", "fix it"}, nil, tty, lookup)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	launch := launched(t, d)
	if launch.Name != "robin" {
		t.Errorf("name = %q, want the first free name robin", launch.Name)
	}
	checkIdentity(t, launch, "robin")
	if want := []string{"--name", "robin", "fix it"}; !slices.Equal(launch.Argv, want) {
		t.Errorf("argv = %q, want the --bead pair removed: %q", launch.Argv, want)
	}
}

func TestPlanSkipsAPoolNameThatIsNotAName(t *testing.T) {
	t.Parallel()
	lookup := newFakeLookup()
	lookup.names = []string{"not a name", "robin"}
	d, err := Plan([]string{"fix it"}, nil, tty, lookup)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if launch := launched(t, d); launch.Name != "robin" {
		t.Errorf("name = %q, want the first usable name robin", launch.Name)
	}
}

func TestPlanKeepsAResumeNameThatIsOurs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		resume string
		lookup *fakeLookup
	}{
		{"in the pool", "lark", newFakeLookup()},
		{"bound to a bead", "dipper", bindFake("gt-9", "dipper")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan([]string{"--resume", tc.resume, "keep going"}, nil, tty, tc.lookup)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			launch := launched(t, d)
			if launch.Name != tc.resume {
				t.Errorf("name = %q, want %q", launch.Name, tc.resume)
			}
			checkIdentity(t, launch, tc.resume)
			want := []string{"--name", tc.resume, "--resume", tc.resume, "keep going"}
			if !slices.Equal(launch.Argv, want) {
				t.Errorf("argv = %q, want %q", launch.Argv, want)
			}
		})
	}
}

func TestPlanPassesThroughAResumeNameThatIsNotOurs(t *testing.T) {
	t.Parallel()
	d, err := Plan([]string{"--resume", "stranger", "keep going"}, nil, tty, newFakeLookup())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	passedThrough(t, d)
}

// TestPlanPassthroughDropsOnlyItsOwnBeadFlag pins what a passthrough hands
// claude: the caller's arguments as written, minus --bead, which is gt's flag
// and not one claude has (gt-qipl5).
func TestPlanPassthroughDropsOnlyItsOwnBeadFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"a spaced --bead and its value", []string{"--bead", "gt-1", "-p", "fix it"}, []string{"-p", "fix it"}},
		{"an inline --bead", []string{"--bead=gt-1", "-p", "fix it"}, []string{"-p", "fix it"}},
		{"another flag keeps the form it was written in", []string{"--name=robin", "--bead=gt-1", "-p"}, []string{"--name=robin", "-p"}},
		{"an inline --resume keeps the value claude reads", []string{"--resume=robin", "-p"}, []string{"--resume=robin", "-p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(tc.args, nil, tty, newFakeLookup())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			passedThrough(t, d)
			if !slices.Equal(d.Passthrough, tc.want) {
				t.Errorf("passthrough = %q, want %q", d.Passthrough, tc.want)
			}
		})
	}
}

func TestPlanRefusesABadBead(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--bead", "gt 1", "fix it"},
		{"--bead", "gt/1"},
		{"--bead", ""},
		{"--bead=gt 1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			d, err := Plan(args, nil, tty, newFakeLookup())
			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("Plan(%q) = %v, want a *UsageError", args, err)
			}
			if got := usage.ExitStatus(); got != 2 {
				t.Errorf("exit status = %d, want 2", got)
			}
			if d.Launch != nil {
				t.Errorf("Plan launched claude as %q, want no launch", d.Launch.Argv)
			}
		})
	}
}

func TestPlanPassesThroughWhenEveryNameIsTaken(t *testing.T) {
	t.Parallel()
	lookup := newFakeLookup()
	for _, name := range lookup.names {
		lookup.taken[name] = true
	}
	d, err := Plan([]string{"fix it"}, nil, tty, lookup)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	passedThrough(t, d)
	if d.Stderr == "" {
		t.Error("Plan passed through on an exhausted pool without a message")
	}
}

func TestPlanDoesNotMistakeAFlagValueForASubcommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"--name value", []string{"--name", "robin"}},
		{"--bead value", []string{"--bead", "gt-1"}},
		{"--resume value", []string{"--resume", "lark"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(tc.args, nil, tty, newFakeLookup())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			launched(t, d)
		})
	}
}

func TestPlanReportsARegistryError(t *testing.T) {
	t.Parallel()
	boom := errors.New("the registry is unreadable")
	for _, tc := range []struct {
		name   string
		args   []string
		lookup *fakeLookup
	}{
		{"bound", []string{"--bead", "gt-1", "fix it"}, &fakeLookup{boundErr: boom}},
		{"owns", []string{"--resume", "lark", "keep going"}, &fakeLookup{ownsErr: boom}},
		{"taken", []string{"fix it"}, &fakeLookup{takenErr: boom}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := Plan(tc.args, nil, tty, tc.lookup)
			if !errors.Is(err, boom) {
				t.Fatalf("Plan error = %v, want it to wrap %v", err, boom)
			}
			if d.Launch != nil {
				t.Errorf("Plan launched claude as %q despite the error", d.Launch.Argv)
			}
		})
	}
}

func TestPlanReplacesAnInheritedGitIdentity(t *testing.T) {
	t.Parallel()
	env := []string{"PATH=/bin", "GIT_AUTHOR_NAME=someone", "GIT_COMMITTER_EMAIL=someone@example.com"}
	d, err := Plan([]string{"fix it"}, env, tty, newFakeLookup())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	launch := launched(t, d)
	checkIdentity(t, launch, "wren")
	if !slices.Contains(launch.Env, "PATH=/bin") {
		t.Errorf("env %q lost PATH", launch.Env)
	}
}

func TestPlanLeavesTheCallersInputsAlone(t *testing.T) {
	t.Parallel()
	args := []string{"--bead", "gt-1", "--name=robin", "fix it"}
	env := []string{"PATH=/bin", "GIT_AUTHOR_NAME=someone"}
	wantArgs, wantEnv := slices.Clone(args), slices.Clone(env)
	if _, err := Plan(args, env, tty, bindFake("gt-1", "lark")); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !slices.Equal(args, wantArgs) {
		t.Errorf("args = %q, want them unchanged: %q", args, wantArgs)
	}
	if !slices.Equal(env, wantEnv) {
		t.Errorf("env = %q, want it unchanged: %q", env, wantEnv)
	}
}
