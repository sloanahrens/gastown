package polecat

import (
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/slot"
)

func TestParsePanes(t *testing.T) {
	t.Parallel()
	out := "gt-slit\t100\nother\t200\ngt-slit\t101\n\ngt-slit\tnotapid\ngt-slit\t0\n"
	if got, want := ParsePanes(out, "gt-slit"), []int{100, 101}; !slices.Equal(got, want) {
		t.Errorf("ParsePanes = %v, want %v", got, want)
	}
	if got := ParsePanes(out, "absent"); len(got) != 0 {
		t.Errorf("ParsePanes for an absent session = %v, want none", got)
	}
	if got := ParsePanes("no tabs here\n", "gt-slit"); len(got) != 0 {
		t.Errorf("ParsePanes of malformed output = %v, want none", got)
	}
}

func TestParseProcessTable(t *testing.T) {
	t.Parallel()
	out := "  PID  PPID ARGS\n" +
		"  1     0 /sbin/launchd\n" +
		"100     1 claude --dangerously-skip-permissions\n" +
		"101   100 make -j4 test\n" +
		"junk line\n" +
		"102   bad ps itself\n" +
		"59375 59370 sh -c echo hi; sleep 60\n"
	got := ParseProcessTable(out)
	want := []Proc{
		{PID: 1, PPID: 0, Args: "/sbin/launchd"},
		{PID: 100, PPID: 1, Args: "claude --dangerously-skip-permissions"},
		{PID: 101, PPID: 100, Args: "make -j4 test"},
		{PID: 59375, PPID: 59370, Args: "sh -c echo hi; sleep 60"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ParseProcessTable =\n%v\nwant\n%v", got, want)
	}
}

func TestDescendantArgvs(t *testing.T) {
	t.Parallel()
	// Pane 100's agent spawns a wrapper (101) that spawns the suite (102);
	// pane 200 is a different polecat and shares no ancestry.
	procs := []Proc{
		{PID: 100, PPID: 1, Args: "pane agent"},
		{PID: 101, PPID: 100, Args: "bash -c make test"},
		{PID: 102, PPID: 101, Args: "go test ./..."},
		{PID: 103, PPID: 1, Args: "unrelated"},
		{PID: 200, PPID: 1, Args: "claude --session-id b"},
		{PID: 210, PPID: 200, Args: "make lint"},
	}
	got := DescendantArgvs([]int{100}, procs)
	want := []string{"bash -c make test", "go test ./..."}
	if !slices.Equal(got, want) {
		t.Errorf("DescendantArgvs = %v, want %v (the root itself and other trees excluded)", got, want)
	}
	if got := DescendantArgvs([]int{999}, procs); len(got) != 0 {
		t.Errorf("DescendantArgvs of an absent root = %v, want none", got)
	}
}

func TestGateTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv string
		want []string
	}{
		{"plain words", "make -j4 test", []string{"make", "-j4", "test"}},
		{"quotes are boundaries", "sh -c \"make test\"", []string{"sh", "-c", "make", "test"}},
		{"grouping parentheses are boundaries", "bash -c (make test)", []string{"bash", "-c", "make", "test"}},
		{"separators stand alone", "make clean&&make test", []string{"make", "clean", "&", "&", "make", "test"}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GateTokens(tt.argv); !slices.Equal(got, tt.want) {
				t.Errorf("GateTokens(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}

func TestInvocationFromCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    Invocation
		wantOK  bool
	}{
		{"plain", "make test", Invocation{Program: "make", Target: "test"}, true},
		{"env prefix", "GOFLAGS=-p=8 make test", Invocation{Program: "make", Target: "test"}, true},
		{"flags before the target", "make -j4 test", Invocation{Program: "make", Target: "test"}, true},
		{"a separate -j value is not the target", "make -j 4 test", Invocation{Program: "make", Target: "test"}, true},
		{"-j without a number leaves the goal", "make -j test", Invocation{Program: "make", Target: "test"}, true},
		{"-C takes its directory", "make -C sub test", Invocation{Program: "make", Target: "test"}, true},
		{"-f takes its file", "make -f gate.mk gate", Invocation{Program: "make", Target: "gate"}, true},
		{"-I takes its directory", "make -I inc test", Invocation{Program: "make", Target: "test"}, true},
		{"the GNU make spelling takes its value", "gmake -C sub test", Invocation{Program: "gmake", Target: "test"}, true},
		{"a value flag with nothing after it", "make -C", Invocation{Program: "make"}, true},
		{"path and case", "/Library/Developer/CommandLineTools/usr/bin/MAKE BUILD", Invocation{Program: "make", Target: "build"}, true},
		{"a program with no target still reports", "pytest -q", Invocation{Program: "pytest"}, true},
		{"nothing to run", "FOO=bar", Invocation{}, false},
		{"empty", "   ", Invocation{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := InvocationFromCommand(GateTokens, tt.command)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("InvocationFromCommand(%q) = %+v, %v; want %+v, %v", tt.command, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSuiteArgv(t *testing.T) {
	t.Parallel()
	suites := []Invocation{
		{Program: "make", Target: "gate"},
		{Program: "make", Target: "test"},
		{Program: "go", Target: "test"},
		{Program: "go", Target: "vet"},
		{Program: "gt", Target: "done"},
	}

	tests := []struct {
		name string
		argv string
		want string
	}{
		{"a bare gate", "make gate", "make gate"},
		{"a make in the middle of a shell body", "/bin/sh -c make test", "make test"},
		{"a background wrapper", "bash -c eval 'gt slot run --role gastown/slit -- make gate' < /dev/null", "gt slot run"},
		{"a wrapper with no slot", "sh -c \"make -j4 test\"", "make test"},
		{"a separate -j value before the goal", "make -j 4 test", "make test"},
		{"a separate -C directory before the goal", "make -C sub test", "make test"},
		{"the Go quality gate", "go test ./internal/daemon -run TestOverrun", "go test"},
		{"go vet at the path it was exec'd with", "/usr/local/go/bin/go vet ./...", "go vet"},
		{"a backgrounded gt done still running", "/usr/local/bin/gt done", "gt done"},
		{"make after a separator is a goal", "make clean && make test", "make test"},
		{"the GNU make spelling", "gmake gate", "make gate"},
		{"gmake's goals read as make's", "/usr/local/bin/gmake clean test", "make test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SuiteArgv(tt.argv, GateTokens, suites)
			if !ok || got != tt.want {
				t.Errorf("SuiteArgv(%q) = %q, %v; want %q, true", tt.argv, got, ok, tt.want)
			}
		})
	}

	notSuites := []string{
		"claude --dangerously-skip-permissions",
		"git status",
		"go build ./...",
		"make",
		"gt slot status",
		"",
	}
	for _, argv := range notSuites {
		if got, ok := SuiteArgv(argv, GateTokens, suites); ok {
			t.Errorf("SuiteArgv(%q) = %q, want no suite", argv, got)
		}
	}
}

// TestSuiteArgvTargetlessGate pins the reading a rig whose config names a bare
// program gets: the program matches anywhere in the argv, which is the Stop
// hook's reading of a targetless command.
func TestSuiteArgvTargetlessGate(t *testing.T) {
	t.Parallel()
	suites := []Invocation{{Program: "pytest"}}
	if got, ok := SuiteArgv("pytest -q --maxfail=1", GateTokens, suites); !ok || got != "pytest" {
		t.Errorf("SuiteArgv = %q, %v; want %q, true", got, ok, "pytest")
	}
}

// TestInvocationPresent_Gmake covers gt-gy5ro: GNU make answers to gmake, the
// name it carries on macOS, so a seat running "gmake test" is running the rig's
// configured "make test". Read as absent, the Stop hook saw no live suite under
// that seat and ran gt done over it while the suite was still going.
func TestInvocationPresent_Gmake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tokens []string
		inv    Invocation
		want   bool
	}{
		{"gmake for a configured make", []string{"gmake", "test"}, Invocation{Program: "make", Target: "test"}, true},
		{"gmake's goals read as make's", []string{"gmake", "clean", "test"}, Invocation{Program: "make", Target: "test"}, true},
		{"make still matches make", []string{"make", "test"}, Invocation{Program: "make", Target: "test"}, true},
		{"a targetless make matches gmake too", []string{"gmake", "test"}, Invocation{Program: "make"}, true},
		{"another program keeps the exact word", []string{"cmake", "test"}, Invocation{Program: "make", Target: "test"}, false},
		{"a gmake token does not match another program", []string{"gmake", "test"}, Invocation{Program: "pytest", Target: "test"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := InvocationPresent(tt.tokens, tt.inv); got != tt.want {
				t.Errorf("InvocationPresent(%v, %+v) = %v, want %v", tt.tokens, tt.inv, got, tt.want)
			}
		})
	}
}

func TestSlotHeldBy(t *testing.T) {
	t.Parallel()
	rep := slot.Report{Slots: []slot.SlotState{
		{Index: 0, Held: true, Owner: &slot.Owner{Role: "gastown/citrine"}},
		{Index: 1, Held: true, Owner: &slot.Owner{Role: "gastown/polecats/coral"}},
	}}

	held, ok := SlotHeldBy(rep, []string{"gastown/coral", "gastown/polecats/coral"})
	if !ok || held.Index != 1 {
		t.Errorf("SlotHeldBy = %+v, %v; want the seat's second role's slot 1", held, ok)
	}
	if _, ok := SlotHeldBy(rep, []string{"gastown/absent"}); ok {
		t.Error("SlotHeldBy found a slot for a role that holds none")
	}
	if _, ok := SlotHeldBy(slot.Report{}, []string{"gastown/coral"}); ok {
		t.Error("SlotHeldBy found a slot in an empty report")
	}
}

// fakeSuiteReads answers a live-suite question from canned reads.
type fakeSuiteReads struct {
	rep      slot.Report
	repErr   error
	panes    []int
	panesErr error
	procs    []Proc
	procsErr error
}

func (f fakeSuiteReads) Slots() (slot.Report, error)    { return f.rep, f.repErr }
func (f fakeSuiteReads) PanePIDs(string) ([]int, error) { return f.panes, f.panesErr }
func (f fakeSuiteReads) ProcessTable() ([]Proc, error)  { return f.procs, f.procsErr }

func TestLiveSuite(t *testing.T) {
	t.Parallel()
	suite := []Invocation{{Program: "make", Target: "test"}}
	held := slot.Report{Slots: []slot.SlotState{{Index: 0, Held: true, Owner: &slot.Owner{Role: "gastown/coral"}}}}
	agent := Proc{PID: 100, PPID: 1, Args: "claude --session-id a"}
	running := Proc{PID: 101, PPID: 100, Args: "make test"}

	t.Run("a slot held by the seat's own role", func(t *testing.T) {
		got, err := LiveSuite(fakeSuiteReads{rep: held}, SuiteQuery{Roles: []string{"gastown/coral"}})
		if err != nil || !got.Busy() || !got.Slot.Held || got.Invocation != "" {
			t.Errorf("LiveSuite = %+v, %v; want the seat's own slot", got, err)
		}
	})

	t.Run("a slot held by another role", func(t *testing.T) {
		got, err := LiveSuite(fakeSuiteReads{rep: held, panes: []int{100}, procs: []Proc{agent, running}},
			SuiteQuery{Roles: []string{"gastown/other"}, Session: "gt-coral", Suites: suite})
		if err != nil || !got.Busy() || got.Invocation != "make test" {
			t.Errorf("LiveSuite = %+v, %v; want the pane's suite", got, err)
		}
	})

	t.Run("a suite in the pane", func(t *testing.T) {
		got, err := LiveSuite(fakeSuiteReads{panes: []int{100}, procs: []Proc{agent, running}},
			SuiteQuery{Roles: []string{"gastown/coral"}, Session: "gt-coral", Suites: suite})
		if err != nil || got.Invocation != "make test" {
			t.Errorf("LiveSuite = %+v, %v; want make test", got, err)
		}
	})

	t.Run("an idle pane", func(t *testing.T) {
		got, err := LiveSuite(fakeSuiteReads{panes: []int{100}, procs: []Proc{agent, {PID: 101, PPID: 100, Args: "git status"}}},
			SuiteQuery{Roles: []string{"gastown/coral"}, Session: "gt-coral", Suites: suite})
		if err != nil || got.Busy() {
			t.Errorf("LiveSuite = %+v, %v; want nothing", got, err)
		}
	})

	t.Run("a session with no pane", func(t *testing.T) {
		if _, err := LiveSuite(fakeSuiteReads{}, SuiteQuery{Session: "gt-coral"}); err == nil {
			t.Error("LiveSuite = nil error; want one, so each caller maps it its own way")
		}
	})

	t.Run("an unreadable slot state", func(t *testing.T) {
		if _, err := LiveSuite(fakeSuiteReads{repErr: errors.New("flock is gone")}, SuiteQuery{}); err == nil {
			t.Error("LiveSuite = nil error; want one")
		}
	})

	// The slot read is asked first, so it is the read a failing host takes out
	// first; the pane scan it would have short-circuited is the only sight a
	// caller has of a suite holding no slot (gt-a1xhd).
	t.Run("a slot read that failed still leaves the pane scan", func(t *testing.T) {
		got, err := LiveSuite(fakeSuiteReads{repErr: errors.New("flock is gone"), panes: []int{100}, procs: []Proc{agent, running}},
			SuiteQuery{Roles: []string{"gastown/coral"}, Session: "gt-coral", Suites: suite})
		if err == nil {
			t.Error("LiveSuite = nil error; want the failed read reported with what was found")
		}
		if got.Invocation != "make test" {
			t.Errorf("LiveSuite = %+v; want the pane's make test, not a blank answer for an unreadable slot state", got)
		}
	})

	t.Run("an unreadable pane list", func(t *testing.T) {
		if _, err := LiveSuite(fakeSuiteReads{panesErr: errors.New("tmux is gone")}, SuiteQuery{Session: "gt-coral"}); err == nil {
			t.Error("LiveSuite = nil error; want one")
		}
	})

	t.Run("an unreadable process table", func(t *testing.T) {
		if _, err := LiveSuite(fakeSuiteReads{panes: []int{100}, procsErr: errors.New("ps is gone")}, SuiteQuery{Session: "gt-coral"}); err == nil {
			t.Error("LiveSuite = nil error; want one")
		}
	})
}

func TestConfiguredCommands(t *testing.T) {
	t.Parallel()
	// A town with no rig config names no commands; what the guards do about
	// that (each carries its own bare invocations) is theirs to say.
	if got := ConfiguredCommands(t.TempDir(), "gastown"); len(got) != 0 {
		t.Errorf("ConfiguredCommands = %v, want none for a town with no config", got)
	}
}
