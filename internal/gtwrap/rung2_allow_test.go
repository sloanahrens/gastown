package gtwrap

import (
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

// rung2ForbiddenWords name a command that starts, stops, restarts or repairs the
// daemon or Dolt, or the start and stop verbs of rigs, crews and sessions. The
// cobra root of internal/cmd is not exported, so this test cannot walk the command
// tree from gtwrap without changing that package. The list is literal instead, as
// the bead allows.
var rung2ForbiddenWords = []string{"daemon", "dolt", "up", "down", "start", "stop", "restart", "repair"}

// rung2ForbiddenWord returns the forbidden word argv names, if it names one.
func rung2ForbiddenWord(argv []string) (string, bool) {
	for _, word := range argv {
		if slices.Contains(rung2ForbiddenWords, word) {
			return word, true
		}
	}
	return "", false
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

func TestRung2TableHasNoDaemonOrDoltVerb(t *testing.T) {
	t.Parallel()
	for _, prefix := range rung2Allowlist {
		if word, bad := rung2ForbiddenWord(prefix); bad {
			t.Errorf("the allowlist names %q, which starts, stops, restarts or repairs the daemon or Dolt (%q)", strings.Join(prefix, " "), word)
		}
	}
}

func TestRung2ForbiddenWordFlagsTheVerbs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		argv []string
		want bool
	}{
		{[]string{"gt", "daemon", "start"}, true},
		{[]string{"gt", "dolt", "status"}, true},
		{[]string{"gt", "up"}, true},
		{[]string{"gt", "down"}, true},
		{[]string{"gt", "rig", "restart"}, true},
		{[]string{"gt", "crew", "stop"}, true},
		{[]string{"gt", "session", "start"}, true},
		{[]string{"gt", "dolt", "repair"}, true},
		{[]string{"gt", "status"}, false},
		{[]string{"gt", "mail", "inbox"}, false},
		{[]string{"bd", "ready"}, false},
	}
	for _, tt := range tests {
		if _, got := rung2ForbiddenWord(tt.argv); got != tt.want {
			t.Errorf("rung2ForbiddenWord(%q) = %v, want %v", tt.argv, got, tt.want)
		}
	}
}
