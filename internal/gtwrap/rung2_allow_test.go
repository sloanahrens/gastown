package gtwrap

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rung2Case is one call the rung-2 decision covers, and whether it runs there.
type rung2Case struct {
	name    string
	argv    []string
	allowed bool
}

// rung2Cases decides each call under rung 2. The first seven rows are the calls
// the design names; they run. Every other row is refused.
var rung2Cases = []rung2Case{
	{"gt status", []string{"gt", "status"}, true},
	{"gt prime", []string{"gt", "prime"}, true},
	{"gt hook", []string{"gt", "hook"}, true},
	{"gt mail inbox", []string{"gt", "mail", "inbox"}, true},
	{"bd show", []string{"bd", "show", "gt-abc"}, true},
	{"bd list", []string{"bd", "list"}, true},
	{"bd ready", []string{"bd", "ready"}, true},
	{"gt status with a flag after the verb", []string{"gt", "status", "--json"}, true},
	{"gt statusx", []string{"gt", "statusx"}, false},
	{"a flag before the verb", []string{"gt", "--json", "status"}, false},
	{"gt mail with no verb", []string{"gt", "mail"}, false},
	{"gt mail send", []string{"gt", "mail", "send", "--stdin"}, false},
	{"gt daemon start", []string{"gt", "daemon", "start"}, false},
	{"gt dolt start", []string{"gt", "dolt", "start"}, false},
	{"gt up", []string{"gt", "up"}, false},
	{"gt down", []string{"gt", "down"}, false},
	{"gt sling", []string{"gt", "sling", "gt-abc", "gastown"}, false},
	{"bd create", []string{"bd", "create", "--title", "t"}, false},
	{"the tool alone", []string{"gt"}, false},
	{"om review", []string{"om", "review"}, false},
	{"hm status", []string{"hm", "status"}, false},
	{"an unknown tool", []string{"ls", "-la"}, false},
}

// rung2ReadOnlyCalls is the whole rung-2 table as docs/design/gt-in-container.md
// names it: the seven read-only calls. The allowlist must equal this set, so no
// command the design does not name can run beside a daemon: a daemon or Dolt
// start, stop, restart or repair verb fails this check as every other addition
// does. A list of forbidden words cannot hold that line, because it misses each
// verb nobody wrote down.
var rung2ReadOnlyCalls = [][]string{
	{"gt", "status"},
	{"gt", "prime"},
	{"gt", "hook"},
	{"gt", "mail", "inbox"},
	{"bd", "show"},
	{"bd", "list"},
	{"bd", "ready"},
}

// TestRung2AllowlistIsExactlyTheReadOnlyCalls refuses any table the design page
// does not name, so a call reaches rung 2 only when the design and the test name
// it together.
func TestRung2AllowlistIsExactlyTheReadOnlyCalls(t *testing.T) {
	t.Parallel()
	if !slices.EqualFunc(rung2Allowlist, rung2ReadOnlyCalls, slices.Equal) {
		t.Errorf("rung2Allowlist = %q, want exactly %q, the read-only calls the design page names; a new call is a policy change that updates rung2_allow.go's table, this set and docs/design/gt-in-container.md together", rung2Allowlist, rung2ReadOnlyCalls)
	}
}

// TestRung2TheOnlyExportedCallRunnersAreExecAndLaunch pins Docker's exported
// surface for running a call: Launch, which holds the rung-2 allowlist gate, and
// Exec, which runs in the already-running container and is not gated. The
// throwaway run is unexported, so no caller outside this package can reach one
// without Launch; a new exported runner must be added here and gated.
func TestRung2TheOnlyExportedCallRunnersAreExecAndLaunch(t *testing.T) {
	t.Parallel()
	runner := reflect.TypeOf((func(Docker, Call) (int, error))(nil))
	var got []string
	typ := reflect.TypeOf(Docker{})
	for i := 0; i < typ.NumMethod(); i++ {
		if m := typ.Method(i); m.Type == runner {
			got = append(got, m.Name)
		}
	}
	if want := []string{"Exec", "Launch"}; !slices.Equal(got, want) {
		t.Fatalf("Docker's exported call runners = %q, want %q; a throwaway run is reached only through Launch, which holds the rung-2 allowlist gate, so a new exported runner must be gated and named here", got, want)
	}
}

func TestRung2RunsTheAllowlistedCalls(t *testing.T) {
	t.Parallel()
	for _, tt := range rung2Cases {
		if !tt.allowed {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(Call{Argv: tt.argv, Dir: testDir})
			if err != nil || exit != 0 {
				t.Fatalf("Launch = %d, %v; want 0, nil", exit, err)
			}
			if len(shell.attached) != 1 {
				t.Fatalf("attached %d lines, want one throwaway run", len(shell.attached))
			}
			line := strings.Join(shell.attached[0], " ")
			want := "--entrypoint " + tt.argv[0] + " " + testImage + " " + strings.Join(tt.argv[1:], " ")
			if verbOf(shell.attached[0]) != "run" || !strings.HasSuffix(line, want) {
				t.Fatalf("line %q does not run %q in the throwaway container; want the suffix %q", line, tt.argv, want)
			}
		})
	}
}

func TestRung2RefusesEveryOtherCall(t *testing.T) {
	t.Parallel()
	start := "docker compose -p gt-live -f " + testComposePath + " up -d daemon"
	for _, tt := range rung2Cases {
		if tt.allowed {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			exit, err := d.Launch(Call{Argv: tt.argv, Dir: testDir})
			if err == nil || exit == 0 {
				t.Fatalf("Launch = %d, %v; want a refusal with a non-zero exit", exit, err)
			}
			for _, want := range []string{strings.Join(tt.argv, " "), start} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
			if len(shell.attached) != 0 {
				t.Fatalf("refused call started the container: attached %q", shell.attached)
			}
		})
	}
}

func TestRung2RefusesACallWithNoArguments(t *testing.T) {
	t.Parallel()
	shell := &fakeShell{}
	d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
	exit, err := d.Launch(Call{Dir: testDir})
	if err == nil || exit == 0 {
		t.Fatalf("Launch = %d, %v; want a refusal with a non-zero exit", exit, err)
	}
	if len(shell.captured) != 0 || len(shell.attached) != 0 {
		t.Fatalf("a call with no arguments reached docker: probes %q, calls %q", shell.captured, shell.attached)
	}
}

// TestRung2GatesOnlyTheThrowawayRung: a running daemon container is rung 1, and
// the allowlist does not apply there. Every refused call runs in it.
func TestRung2GatesOnlyTheThrowawayRung(t *testing.T) {
	t.Parallel()
	for _, tt := range rung2Cases {
		if tt.allowed {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			shell := &fakeShell{psOut: "5c1e0a7f\n"}
			d := Docker{Town: testTown, Shell: shell, ReadFile: readCompose(testCompose)}
			if exit, err := d.Launch(Call{Argv: tt.argv, Dir: testDir}); err != nil || exit != 0 {
				t.Fatalf("Launch = %d, %v; want the exec rung to run it", exit, err)
			}
			if len(shell.attached) != 1 || verbOf(shell.attached[0]) != "exec" {
				t.Fatalf("attached %q, want one exec line", shell.attached)
			}
		})
	}
}
