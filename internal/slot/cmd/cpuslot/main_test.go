package main

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// recorder is a host whose every collaborator records the call it received, so
// a case asserts which one runFrom chose.
type recorder struct {
	getwd  func() (string, error)
	find   func(string) (string, error)
	ran    [][]string
	queued [][]string
	warned strings.Builder
}

func (r *recorder) host() host {
	return host{
		getwd: r.getwd,
		find:  r.find,
		run: func(args []string) (int, error) {
			r.ran = append(r.ran, args)
			return 42, nil
		},
		queued: func(townRoot, role string, args []string) (int, error) {
			r.queued = append(r.queued, append([]string{townRoot, role}, args...))
			return 43, nil
		},
		warn: &r.warned,
	}
}

// runErr calls runFrom with no working directory and a find that must not be
// reached, so each refusal case reads as the input it supplies.
func runErr(t *testing.T, args []string, getwd func() (string, error)) string {
	t.Helper()
	r := &recorder{getwd: getwd, find: notReached(t)}
	_, err := runFrom(args, r.host())
	if err == nil {
		t.Fatal("runFrom returned no error, want a refusal")
	}
	return err.Error()
}

// TestCPUClassWrapperRefusesNoCommand: an invocation naming no command is
// refused before any working directory or town is read.
// Mutation: drop the length check, and the refusal becomes a start of "".
func TestCPUClassWrapperRefusesNoCommand(t *testing.T) {
	t.Parallel()
	r := &recorder{getwd: notRead(t), find: notReached(t)}
	if _, err := runFrom(nil, r.host()); err == nil || !strings.Contains(err.Error(), "no command given") {
		t.Fatalf("error = %v, want it to name the missing command", err)
	}
}

// TestCPUClassWrapperRefusesUnreadableWorkingDirectory: a working directory
// that cannot be read refuses rather than running the command from a tree it
// cannot name.
// Mutation: run the command unslotted on this error, and the refusal becomes a
// build that never took a slot.
func TestCPUClassWrapperRefusesUnreadableWorkingDirectory(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "", errors.New("cwd is gone") }
	got := runErr(t, []string{"go", "build"}, getwd)
	if !strings.Contains(got, "reading the working directory") {
		t.Fatalf("error = %q, want it to name the working directory", got)
	}
}

// TestCPUClassWrapperRunsUnslottedWhenTheTownLookupFails: a lookup that errors
// is not a crew checkout, so the command runs unchanged rather than failing a
// build over the class's own books. The notice names the failed lookup.
// Mutation: refuse on this error, and the landing worker's gate and CI fail
// whenever the tree cannot be walked.
func TestCPUClassWrapperRunsUnslottedWhenTheTownLookupFails(t *testing.T) {
	t.Parallel()
	r := &recorder{
		getwd: func() (string, error) { return "/town/gastown/crew/olive", nil },
		find:  func(string) (string, error) { return "", errors.New("town.json unreadable") },
	}
	code, err := runFrom([]string{"go", "build", "./..."}, r.host())
	if err != nil || code != 42 {
		t.Fatalf("runFrom = (%d, %v), want the unchanged command's (42, nil)", code, err)
	}
	if want := [][]string{{"go", "build", "./..."}}; !slices.EqualFunc(r.ran, want, slices.Equal) {
		t.Fatalf("commands run unchanged = %v, want %v", r.ran, want)
	}
	if len(r.queued) != 0 {
		t.Fatalf("a command queued on a failed lookup: %v", r.queued)
	}
	for _, want := range []string{"town.json unreadable", "without a CPU-class slot"} {
		if !strings.Contains(r.warned.String(), want) {
			t.Errorf("notice = %q, want it to name %q", r.warned.String(), want)
		}
	}
}

// TestCPUClassWrapperRunsUnslottedOutsideACrewCheckout: a tree that is not a
// crew worktree — no town, a polecat checkout, the crew directory itself — runs
// the command unchanged, so polecats and the landing gate keep their path.
// Mutation: queue anything inside a town, and a polecat's build waits behind
// the crew class.
func TestCPUClassWrapperRunsUnslottedOutsideACrewCheckout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := []struct {
		name string
		cwd  string
		root string
	}{
		{name: "no town", cwd: t.TempDir()},
		{name: "polecat checkout", cwd: town + "/gastown/polecats/dag/gastown", root: town},
		{name: "the crew directory itself", cwd: town + "/gastown/crew", root: town},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{
				getwd: func() (string, error) { return tc.cwd, nil },
				find:  func(string) (string, error) { return tc.root, nil },
			}
			code, err := runFrom([]string{"go", "vet", "./..."}, r.host())
			if err != nil || code != 42 {
				t.Fatalf("runFrom = (%d, %v), want the unchanged command's (42, nil)", code, err)
			}
			if len(r.queued) != 0 {
				t.Fatalf("a command outside a crew checkout queued: %v", r.queued)
			}
			if len(r.ran) != 1 {
				t.Fatalf("commands run unchanged = %v, want exactly one", r.ran)
			}
		})
	}
}

// TestCPUClassWrapperQueuesFromACrewCheckout: a crew checkout's command runs
// under the class, tagged with the crew role.
// Mutation: run the command unslotted in a crew checkout, and a burst of crew
// builds piles onto the host beside its neighbours.
func TestCPUClassWrapperQueuesFromACrewCheckout(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cwd := town + "/gastown/crew/olive/internal/slot"
	r := &recorder{
		getwd: func() (string, error) { return cwd, nil },
		find:  func(string) (string, error) { return town, nil },
	}
	code, err := runFrom([]string{"GT_TEST_DOCKER=0", "go", "test", "./..."}, r.host())
	if err != nil || code != 43 {
		t.Fatalf("runFrom = (%d, %v), want the queued command's (43, nil)", code, err)
	}
	want := [][]string{{town, "gastown/crew/olive", "GT_TEST_DOCKER=0", "go", "test", "./..."}}
	if !slices.EqualFunc(r.queued, want, slices.Equal) {
		t.Fatalf("queued = %v, want %v", r.queued, want)
	}
	if len(r.ran) != 0 {
		t.Fatalf("a crew checkout's command ran unslotted: %v", r.ran)
	}
}

// TestWithCoreShare is the CPU-class build's core bound: a build that names no
// GOMAXPROCS of its own gains one carrying the slot's share, a build that sets
// its own keeps it, and a share below one is floored at one so a bad value
// cannot leave the build unbounded.
func TestWithCoreShare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		env   []string
		share int
		want  []string
	}{
		{
			name:  "adds the share when the build sets none",
			env:   []string{"PATH=/bin"},
			share: 8,
			want:  []string{"PATH=/bin", "GOMAXPROCS=8"},
		},
		{
			name:  "keeps a GOMAXPROCS the build sets itself",
			env:   []string{"PATH=/bin", "GOMAXPROCS=4"},
			share: 8,
			want:  []string{"PATH=/bin", "GOMAXPROCS=4"},
		},
		{
			name:  "floors a share below one",
			env:   []string{"PATH=/bin"},
			share: 0,
			want:  []string{"PATH=/bin", "GOMAXPROCS=1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := withCoreShare(tc.env, tc.share); !slices.Equal(got, tc.want) {
				t.Fatalf("withCoreShare(%v, %d) = %v, want %v", tc.env, tc.share, got, tc.want)
			}
		})
	}
}

// TestWithCoreShareDoesNotModifyItsArgument: the caller's environment is read
// only. The slice has spare capacity and entries past its length, so an
// implementation that appends onto it writes into the backing array and is
// caught; a zero-capacity literal would hide that.
// Mutation: append directly onto env, and the backing-array check goes red.
func TestWithCoreShareDoesNotModifyItsArgument(t *testing.T) {
	t.Parallel()
	backing := []string{"PATH=/bin", "SENTINEL=a", "SENTINEL=b"}
	env := backing[:1]
	got := withCoreShare(env, 8)
	if want := []string{"PATH=/bin", "GOMAXPROCS=8"}; !slices.Equal(got, want) {
		t.Fatalf("withCoreShare = %v, want %v", got, want)
	}
	if want := []string{"SENTINEL=a", "SENTINEL=b"}; !slices.Equal(backing[1:], want) {
		t.Fatalf("withCoreShare wrote past the caller's slice: backing = %v, want %v", backing, want)
	}
	if len(env) != 1 {
		t.Fatalf("withCoreShare changed the argument: %d entries, want 1", len(env))
	}
	if env[0] != "PATH=/bin" {
		t.Fatalf("withCoreShare changed the argument's first entry: %q, want %q", env[0], "PATH=/bin")
	}
}

// TestCPUSlotShare drives runFrom into the queued command on a host with 24
// cores and a class of 3: a crew worktree's child runs with its third of the
// cores unless the build already names a GOMAXPROCS, and a command from any
// other tree takes the unchanged path, which reads no environment here.
// Mutation: hand the class the caller's environment as it is, and the crew row
// loses its GOMAXPROCS.
func TestCPUSlotShare(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := []struct {
		name string
		cwd  string
		env  []string
		want []string // nil: the command runs unchanged
	}{
		{
			name: "crew worktree",
			cwd:  town + "/gastown/crew/olive",
			env:  []string{"PATH=/bin"},
			want: []string{"PATH=/bin", "GOMAXPROCS=8"},
		},
		{
			name: "crew worktree whose build names its own",
			cwd:  town + "/gastown/crew/olive",
			env:  []string{"GOMAXPROCS=4"},
			want: []string{"GOMAXPROCS=4"},
		},
		{
			name: "outside a crew worktree",
			cwd:  town + "/gastown/polecats/dag/gastown",
			env:  []string{"PATH=/bin"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var classes []slot.CPURunOptions
			var sizedFor []string
			r := &recorder{
				getwd: func() (string, error) { return tc.cwd, nil },
				find:  func(string) (string, error) { return town, nil },
			}
			h := r.host()
			h.queued = queuedUnder(time.Minute, machine{
				environ: func() []string { return tc.env },
				numCPU:  24,
				classSize: func(townRoot string) int {
					sizedFor = append(sizedFor, townRoot)
					return 3
				},
				runCPU: func(townRoot string, opts slot.CPURunOptions) (int, error) {
					classes = append(classes, opts)
					return 7, nil
				},
			})

			code, err := runFrom([]string{"go", "test", "./..."}, h)
			if err != nil {
				t.Fatalf("runFrom: %v", err)
			}
			if tc.want == nil {
				if code != 42 || len(r.ran) != 1 || len(classes) != 0 || len(sizedFor) != 0 {
					t.Fatalf("code %d, unchanged runs %v, class runs %d, size reads %v; want only the unchanged command", code, r.ran, len(classes), sizedFor)
				}
				return
			}
			if code != 7 || len(classes) != 1 {
				t.Fatalf("code %d, class runs %d; want the one class run's 7", code, len(classes))
			}
			got := classes[0]
			if got.Environ == nil {
				t.Fatal("the class run names no environment, so the child gets the caller's with no core share")
			}
			if child := got.Environ(); !slices.Equal(child, tc.want) {
				var procs []string
				for _, entry := range child {
					if strings.HasPrefix(entry, "GOMAXPROCS=") {
						procs = append(procs, entry)
					}
				}
				t.Errorf("child has %d entries with %v, want %d entries ending %s", len(child), procs, len(tc.want), tc.want[len(tc.want)-1])
			}
			if got.Size != 3 || !slices.Equal(sizedFor, []string{town}) {
				t.Errorf("class size = %d read for %v, want 3 read once for %s", got.Size, sizedFor, town)
			}
			if got.Role != "gastown/crew/olive" || got.Timeout != time.Minute || !slices.Equal(got.Args, []string{"go", "test", "./..."}) {
				t.Errorf("class run = role %q, timeout %s, args %v; want the caller's", got.Role, got.Timeout, got.Args)
			}
		})
	}
}

// notRead is the working directory a case must not read.
func notRead(t *testing.T) func() (string, error) {
	t.Helper()
	return func() (string, error) {
		t.Error("the case read the working directory it must not reach")
		return "", nil
	}
}

// notReached is the town lookup a case must not run; reaching it fails the
// test rather than letting the case start its command.
func notReached(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(string) (string, error) {
		t.Error("the case read a collaborator it must not reach")
		return "", nil
	}
}
