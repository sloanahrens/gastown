package cmd

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestParseBdCloseInvocations pins the command shape recognition: which
// segments count as a `bd close`, which ids they carry, and which tokens are
// flags or flag values rather than ids.
func TestParseBdCloseInvocations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []bdCloseInvocation
	}{
		{
			name:    "bare close",
			command: "bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			// `bd close --help` lists "done" as an alias; it closes the bead
			// the same way, so the guard has to see it.
			name:    "done alias",
			command: "bd done gt-abc",
			want:    []bdCloseInvocation{{IDs: []string{"gt-abc"}}},
		},
		{
			name:    "done alias with a reason",
			command: `bd done -r "cancel: x" gt-abc`,
			want:    []bdCloseInvocation{{IDs: []string{"gt-abc"}, Reason: "cancel: x"}},
		},
		{
			name:    "done alias behind a global flag",
			command: "bd --db x done gt-abc",
			want:    []bdCloseInvocation{{IDs: []string{"gt-abc"}}},
		},
		{
			name:    "multiple ids",
			command: "bd close gt-arno gt-other",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno", "gt-other"}}},
		},
		{
			name:    "reason flag before id",
			command: `bd close -r "supersede: folded into gt-x" gt-arno`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "supersede: folded into gt-x",
			}},
		},
		{
			name:    "reason flag after id",
			command: `bd close gt-arno --reason "cancel: abandoned"`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "cancel: abandoned",
			}},
		},
		{
			name:    "equals-form reason",
			command: `bd close gt-arno --reason="cancel: not needed"`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "cancel: not needed",
			}},
		},
		{
			// The unquoted space means a shell would split this into two argv
			// entries, so the test pins the parser's own behavior on the
			// mutation rather than a realistic invocation: the glued value is
			// the reason, and the following word must not become an id.
			name:    "glued short reason",
			command: `bd close gt-arno -rsupersede:`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "supersede:",
			}},
		},
		{
			name:    "boolean flags are not ids",
			command: "bd close --force gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "reason is not mistaken for an id",
			command: `bd close --reason "gt-decoy" gt-arno`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "gt-decoy",
			}},
		},
		{
			name:    "found on a later segment of a compound command",
			command: "cd /tmp && bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "env assignment prefix",
			command: "BD_ACTOR=x bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "absolute path to the binary",
			command: "/usr/local/bin/bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "two closes in one line",
			command: "bd close gt-a; bd close gt-b",
			want: []bdCloseInvocation{
				{IDs: []string{"gt-a"}},
				{IDs: []string{"gt-b"}},
			},
		},
		{
			name:    "unrelated bd subcommand",
			command: "bd update gt-arno --status=in_progress",
			want:    nil,
		},
		{
			name:    "close in a heredoc body is data, not a command",
			command: "gt mail send mayor/ -s hi --stdin <<'BODY'\nbd close gt-arno\nBODY",
			want:    nil,
		},
		{
			name:    "close mentioned inside a quoted argument",
			command: `echo "run bd close gt-arno"`,
			want:    nil,
		},
		{
			name:    "similar-looking binary is not bd",
			command: "subd close gt-arno",
			want:    nil,
		},
		{name: "subshell", command: "(bd close gt-arno)", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "brace group", command: "{ bd close gt-arno; }", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind then", command: "if x; then bd close gt-arno; fi", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind if", command: "if bd close gt-arno; then echo ok; fi", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind if !", command: "if ! bd close gt-arno; then echo failed; fi", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind while", command: "while bd close gt-arno; do sleep 1; done", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind until", command: "until bd close gt-arno; do sleep 1; done", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind elif", command: "if x; then y; elif bd close gt-arno; then z; fi", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "an if of something else is not a close", command: "if bd update gt-arno --status=open; then echo ok; fi", want: nil},

		// A case arm is `PATTERN) LIST`. The label is not the command word, so
		// the close in the arm is the one the invariant must judge — whichever
		// arm it sits in, and in every label spelling the shell accepts
		// (gt-37k43).
		{name: "inside the first case arm", command: "case $x in a) bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "inside a parenthesised case label", command: "case $x in (a) bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "alongside a case label alternation", command: "case $x in a|b) bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind a case wildcard label", command: "case $x in (*) bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "inside a later case arm", command: "case $x in a) true ;; b) bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "inside a later parenthesised case arm", command: "case $x in a) true ;; (b) bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "case arm carrying a reason", command: `case $x in a) bd close -r "cancel: x" gt-arno ;; esac`, want: []bdCloseInvocation{{IDs: []string{"gt-arno"}, Reason: "cancel: x"}}},
		{name: "one close per case arm", command: "case $x in a) bd close gt-arno ;; b) bd close gt-other ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}, {IDs: []string{"gt-other"}}}},
		{name: "nested case", command: "case $x in a) case $y in b) bd close gt-arno ;; esac ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "case inside a loop", command: "while read -r x; do case $x in a) bd close gt-arno ;; esac; done", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "subshell inside a case arm", command: "case $x in a) ( bd close gt-arno ) ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "case inside a bash -c payload", command: "bash -c 'case $x in a) bd close gt-arno ;; esac'", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "case arm with a command that is not bd", command: "case $x in a) true ;; b) echo bd close gt-arno ;; esac", want: nil},
		{name: "case arm with a non-close bd subcommand", command: "case $x in a) bd update gt-arno --status=open ;; esac", want: nil},
		{name: "case arm with no close at all", command: "case $x in a) true ;; esac", want: nil},
		// A label glued to its command (`a)bd`) is the same arm as the spaced
		// form; bash accepts it (gt-5toy9).
		{name: "glued first case arm", command: "case $x in a)bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "glued wildcard case arm", command: "case $x in *)bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "glued alternation case arm", command: "case $x in a|b)bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "glued later case arm", command: "case $x in a) true ;; b)bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "glued parenthesised case arm", command: "case $x in (a)bd close gt-arno ;; esac", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "glued case arm carrying a reason", command: `case $x in a)bd close -r "cancel: x" gt-arno ;; esac`, want: []bdCloseInvocation{{IDs: []string{"gt-arno"}, Reason: "cancel: x"}}},
		{name: "glued case arm in a bash -c payload", command: "bash -c 'case $x in a)bd close gt-arno ;; esac'", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "glued case arm with a command that is not bd", command: "case $x in a)echo bd close gt-arno ;; esac", want: nil},
		{name: "glued word outside a case is not an arm", command: "a)bd close gt-arno", want: nil},
		{name: "glued word after a finished case is not an arm", command: "case $x in a) true ;; esac; a)bd close gt-arno", want: nil},
		{name: "glued word as an argument is not an arm", command: "echo a)bd close gt-arno", want: nil},
		// A group's own `)` closes the segment, so the close in a subshell is
		// still the command word rather than a label's tail. The parsed reading
		// has no token for the subshell's parentheses or its redirect, so the
		// close carries its one id; the shell reading used to add `)` and
		// `2>/dev/null` as ids, which no branch can name. Ruling on 0c852cb47f2b:
		// the change is accepted because the dropped ids were never ids, and the
		// bd-close invariant compares bead-shaped ids only (isBeadIDShaped).
		{name: "subshell holding the close keeps it", command: "( bd close gt-arno ) 2>/dev/null", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		// The body is read as a command, as a `{ ...; }` group is. The shell
		// reading missed a close in a function body, so `foo() { bd close
		// gt-arno; }; foo` went unjudged; the parsed reading judges it. Ruling on
		// 0c852cb47f2b: a close the shell reading missed may now be found.
		{name: "function body is not a case arm", command: "foo() { bd close gt-arno; }", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "case inside a quoted argument", command: `echo "case $x in a) bd close gt-arno ;; esac"`, want: nil},
		{name: "case written into a heredoc is data", command: "cat > /tmp/f.sh <<'EOF'\ncase $x in a) bd close gt-arno ;; esac\nEOF", want: nil},
		{name: "behind timeout", command: "timeout 30 bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind timeout with a flag", command: "timeout -s KILL 30 bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind timeout with a kill-after", command: "timeout -k 5 30 bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind nice", command: "nice -n 5 bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind sudo -u", command: "sudo -u alice bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind sudo with two value flags", command: "sudo -u alice -g staff bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind sudo, reason after the launcher", command: `sudo -u alice bd close -r "cancel: x" gt-arno`, want: []bdCloseInvocation{{IDs: []string{"gt-arno"}, Reason: "cancel: x"}}},
		{name: "behind stdbuf -o", command: "stdbuf -o L bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "behind stdbuf with three buffers", command: "stdbuf -i 0 -o L -e L bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		// A flag the table does not name keeps the behavior it had before the
		// table existed: it is dropped on its own, value or no value.
		{name: "behind sudo -E", command: "sudo -E bd close gt-arno", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "a sudo of something else is not a close", command: "sudo -u alice systemctl restart bd", want: nil},
		{name: "a sudo of a non-close bd subcommand is not a close", command: "sudo -u alice bd update gt-arno --status=in_progress", want: nil},
		{name: "a stdbuf of something else is not a close", command: "stdbuf -o L gt status", want: nil},
		{name: "inside bash -c", command: "bash -c 'bd close gt-arno'", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "inside bash -lc", command: `bash -lc "bd close -r 'cancel: x' gt-arno"`, want: []bdCloseInvocation{{IDs: []string{"gt-arno"}, Reason: "cancel: x"}}},
		{name: "a bash -c payload that is not a close", command: "bash -c 'echo bd close gt-arno'", want: nil},
		{name: "a close in a process substitution", command: "cat <(bd close gt-arno)", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		// The shell expands a substitution in an unquoted heredoc body while it
		// reads the body, so it is a close like one on the command line. A quoted
		// tag keeps its body literal, so the same text is data.
		{name: "a close in an unquoted heredoc body", command: "cat <<EOF\n$(bd close gt-arno)\nEOF", want: []bdCloseInvocation{{IDs: []string{"gt-arno"}}}},
		{name: "a close in a quoted heredoc body is data", command: "cat <<'EOF'\n$(bd close gt-arno)\nEOF", want: nil},
		// The parser cannot read this line, so the close inside its command
		// substitution is unknown: it is judged as a close of the branch's bead
		// rather than passed (fail closed). A line with no substitution has
		// nothing to judge.
		{name: "a substitution the parser cannot read is an unknown close", command: `echo $(bd close gt-arno) "unterminated`, want: []bdCloseInvocation{{IDsUnreadable: true}}},
		{name: "a line the parser cannot read with no substitution is not a close", command: `echo "unterminated`, want: nil},
		{
			// A variable id is unreadable, so it cannot match a branch and
			// cannot be judged. It is still reported as an invocation, so
			// "no bd close at all" stays distinguishable from "a close whose
			// ids this parser could not read".
			name:    "variable id is reported but matches nothing",
			command: "bd close $ISSUE",
			want:    []bdCloseInvocation{{IDs: []string{"$ISSUE"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseBdCloseInvocations(tt.command)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseBdCloseInvocations(%q) = %+v, want %+v", tt.command, got, tt.want)
			}
		})
	}
}

// TestBranchNamesBead pins the scope rule that decides which closes this guard
// judges at all: the branch is the only signal that a bead is the source_issue
// of the work in this worktree, so the match has to be exact and component-wise.
func TestBranchNamesBead(t *testing.T) {
	t.Parallel()
	const branch = "polecat/malachite/gt-arno+muck73gu"
	tests := []struct {
		name    string
		issueID string
		want    bool
	}{
		{"the bead the branch was cut for", "gt-arno", true},
		{"case-insensitive", "GT-Arno", true},
		{"leading/trailing whitespace from parsing", " gt-arno ", true},
		{"a prefix of the id is not the id", "gt-arn", false},
		{"a longer id is not the id", "gt-arnold", false},
		{"the suffix component is not a bead id", "muck73gu", false},
		{"the polecat name is not a bead id", "malachite", false},
		{"another bead entirely", "gt-other", false},
		{"a bead id cannot match a plain word", "temp", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := branchNamesBead(branch, tt.issueID); got != tt.want {
				t.Errorf("branchNamesBead(%q, %q) = %v, want %v", branch, tt.issueID, got, tt.want)
			}
		})
	}

	t.Run("default branch names no bead", func(t *testing.T) {
		t.Parallel()
		if branchNamesBead("main", "main") {
			t.Error("a default branch must not be read as naming the bead \"main\"")
		}
	})
}

// TestBdCloseInvariantRefusal pins the decision table at the guard's own
// boundary: the two gt-6hmz exits, and refusal when none holds. It is the
// guard's contract that it refuses exactly what gt done refuses.
func TestBdCloseInvariantRefusal(t *testing.T) {
	t.Parallel()
	scope := func(count int) bdCloseInvariantScope {
		return bdCloseInvariantScope{
			branch:  "polecat/malachite/gt-arno+muck73gu",
			target:  "origin/main",
			counter: fakeCloseTimeCommitCounter{count: count},
		}
	}

	t.Run("exit (a): zero commits ahead", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(0), "gt-arno", ""); got != "" {
			t.Errorf("expected close allowed with zero commits ahead, got %q", got)
		}
	})

	t.Run("exit (b): supersede reason", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(4), "gt-arno", "supersede: folded into gt-x"); got != "" {
			t.Errorf("expected supersede: reason to allow the close, got %q", got)
		}
	})

	t.Run("exit (b): cancel reason", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(4), "gt-arno", "cancel: abandoned"); got != "" {
			t.Errorf("expected cancel: reason to allow the close, got %q", got)
		}
	})

	t.Run("refuses unmerged work with nothing tracking it", func(t *testing.T) {
		t.Parallel()
		got := bdCloseInvariantRefusal(scope(4), "gt-arno", "")
		if got == "" {
			t.Fatal("expected refusal for unmerged commits with no override reason")
		}
		if !strings.Contains(got, "polecat/malachite/gt-arno+muck73gu") {
			t.Errorf("refusal must name the branch, got %q", got)
		}
		if !strings.Contains(got, "4") {
			t.Errorf("refusal must name the unmerged commit count, got %q", got)
		}
	})

	t.Run("a done-style reason is not an override", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(4), "gt-arno", "done"); got == "" {
			t.Error("expected a non-prefix reason to still be refused")
		}
	})
}

// TestRunTapGuardBdCloseInvariant_UnrelatedCommandAllowed pins the self-filter:
// the guard is on every Bash call in the town, so anything that is not a
// bd close must pass without even resolving git scope.
func TestRunTapGuardBdCloseInvariant_UnrelatedCommandAllowed(t *testing.T) {
	t.Parallel()
	proc := guardProcess{getenv: envMap(map[string]string{"GT_POLECAT": "malachite"}), getwd: os.Getwd}

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"ls -la && bd list --status=open"}}`
	err := tapGuardBdCloseInvariant(strings.NewReader(hookInput), proc)
	if err != nil {
		t.Errorf("expected a non-close command to be allowed, got error: %v", err)
	}
}

// TestRealGuardProcessIsTheProcess: runTapGuardBdCloseInvariant hands the
// guard realGuardProcess, which must read the real environment and working
// directory.
func TestRealGuardProcessIsTheProcess(t *testing.T) {
	t.Parallel()
	p := realGuardProcess()
	// The guard's own seam must read the process environment itself, not a
	// stub: PATH is what the process holds, and it must answer with it.
	if got, want := p.getenv("PATH"), os.Getenv("PATH"); got != want {
		t.Errorf("realGuardProcess getenv(PATH) = %q, want the process's %q", got, want)
	}
	if reflect.ValueOf(p.getwd).Pointer() != reflect.ValueOf(os.Getwd).Pointer() {
		t.Error("realGuardProcess does not read the working directory through os.Getwd")
	}
}

func bdCloseIDs(t *testing.T, command string) [][]string {
	t.Helper()
	var out [][]string
	for _, inv := range parseBdCloseInvocations(command) {
		out = append(out, inv.IDs)
	}
	return out
}

// TestParseBdCloseInvocations_SubstitutionEvalXargs: a close run through a
// command substitution, eval or xargs is still a close.
func TestParseBdCloseInvocations_SubstitutionEvalXargs(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, command string }{
		{"dollar-paren substitution", "echo $(bd close gt-x)"},
		{"substitution inside double quotes", `echo "$(bd close gt-x)"`},
		{"backtick substitution", "echo `bd close gt-x`"},
		{"nested substitution", "echo $(echo $(bd close gt-x))"},
		{"eval of a quoted command", "eval 'bd close gt-x'"},
		{"eval of bare words", "eval bd close gt-x"},
		{"xargs fed by echo", "echo gt-x | xargs bd close"},
		{"xargs with flags fed by printf", `printf 'gt-x\n' | xargs -n 1 bd close`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := bdCloseIDs(t, tt.command)
			if len(got) != 1 || !reflect.DeepEqual(got[0], []string{"gt-x"}) {
				t.Errorf("parseBdCloseInvocations(%q) ids = %v, want one invocation holding [gt-x]", tt.command, got)
			}
		})
	}

	notCloses := []string{
		"echo '$(bd close gt-x)'",         // single quotes hide the substitution
		`echo \$(bd close gt-x)`,          // escaped, literal text
		"echo 'bd close gt-x' | xargs ls", // xargs of something else
		"ls | xargs bd close",             // producer unreadable: close reported, no ids
	}
	for _, command := range notCloses {
		for _, ids := range bdCloseIDs(t, command) {
			if len(ids) != 0 {
				t.Errorf("parseBdCloseInvocations(%q) read ids %v, want none", command, ids)
			}
		}
	}
}

// TestParseBdCloseInvocations_EnvFlagsAndPath: env may be a path and carry its
// own flags before the command.
func TestParseBdCloseInvocations_EnvFlagsAndPath(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"/usr/bin/env bd close gt-x",
		"env -i bd close gt-x",
		"env -u FOO bd close gt-x",
		"env -u FOO -i BAR=1 bd close gt-x",
		"env -- bd close gt-x",
		"env FOO=1 bd close gt-x",
	} {
		got := bdCloseIDs(t, command)
		if len(got) != 1 || !reflect.DeepEqual(got[0], []string{"gt-x"}) {
			t.Errorf("parseBdCloseInvocations(%q) ids = %v, want one invocation with [gt-x]", command, got)
		}
	}
	if got := parseBdCloseInvocations("env -u bd echo close gt-x"); len(got) != 0 {
		t.Errorf("env of another command parsed as a close: %+v", got)
	}
}

// TestParseBdCloseInvocations_RepeatedReasonPositional: bd maps the n'th
// --reason to the n'th id, so one id's override must not cover the others.
func TestParseBdCloseInvocations_RepeatedReasonPositional(t *testing.T) {
	t.Parallel()
	scope := bdCloseInvariantScope{
		branch:  "polecat/malachite/gt-a+gt-b",
		target:  "origin/main",
		counter: fakeCloseTimeCommitCounter{count: 3},
	}

	got := parseBdCloseInvocations("bd close gt-a gt-b --reason x --reason 'supersede: a'")
	if len(got) != 1 {
		t.Fatalf("got %d invocations, want 1: %+v", len(got), got)
	}
	if want := []string{"x", "supersede: a"}; !reflect.DeepEqual(got[0].Reasons, want) {
		t.Errorf("Reasons = %q, want %q", got[0].Reasons, want)
	}
	if r := got[0].reasonFor(0); r != "x" {
		t.Errorf("reasonFor(gt-a) = %q, want x", r)
	}
	if r := got[0].reasonFor(1); r != "supersede: a" {
		t.Errorf("reasonFor(gt-b) = %q, want the supersede reason", r)
	}
	if id, refusal := firstBdCloseRefusal(scope, got, ""); id != "gt-a" || refusal == "" {
		t.Errorf("guard refused (%q, %q), want gt-a refused", id, refusal)
	}

	// Reversed: gt-a is overridden, gt-b is the one refused.
	rev := parseBdCloseInvocations("bd close gt-a gt-b --reason 'supersede: a' --reason x")
	if id, refusal := firstBdCloseRefusal(scope, rev, ""); id != "gt-b" || refusal == "" {
		t.Errorf("reversed: guard refused (%q, %q), want gt-b refused", id, refusal)
	}

	// Flags between the ids do not change the pairing.
	mid := parseBdCloseInvocations("bd close gt-a --reason 'supersede: a' gt-b --reason 'cancel: b'")
	if id, refusal := firstBdCloseRefusal(scope, mid, ""); refusal != "" {
		t.Errorf("both ids overridden but guard refused %q: %s", id, refusal)
	}

	// One reason still covers every id; a count that is neither one nor one per
	// id falls back to the last reason, as before.
	one := parseBdCloseInvocations("bd close gt-a gt-b --reason 'supersede: a'")
	if id, refusal := firstBdCloseRefusal(scope, one, ""); refusal != "" {
		t.Errorf("a single supersede reason covers both ids but %q was refused: %s", id, refusal)
	}
	three := parseBdCloseInvocations("bd close gt-a gt-b --reason x --reason y --reason 'supersede: z'")
	if r := three[0].reasonFor(0); r != "supersede: z" {
		t.Errorf("mismatched reason count: reasonFor = %q, want the last reason", r)
	}
}

// TestParseBdCloseInvocations_ReasonFile: --reason-file's path is the flag's
// value, not an id, and the file's reason is what the guard judges.
func TestParseBdCloseInvocations_ReasonFile(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"bd close gt-abc --reason-file /tmp/r.txt",
		"bd close gt-abc --reason-file=/tmp/r.txt",
		"bd close --reason-file /tmp/r.txt gt-abc",
	} {
		got := parseBdCloseInvocations(command)
		if len(got) != 1 || !reflect.DeepEqual(got[0].IDs, []string{"gt-abc"}) || got[0].ReasonFile != "/tmp/r.txt" {
			t.Errorf("parseBdCloseInvocations(%q) = %+v, want ids [gt-abc] and ReasonFile /tmp/r.txt", command, got)
		}
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("supersede.txt", "supersede: folded into gt-y\n")
	write("plain.txt", "done\n")
	scope := bdCloseInvariantScope{
		branch:  "polecat/malachite/gt-abc+muck73gu",
		target:  "origin/main",
		counter: fakeCloseTimeCommitCounter{count: 2},
	}

	tests := []struct {
		name, command, cwd string
		refused            bool
	}{
		{"supersede in the file", "bd close gt-abc --reason-file " + dir + "/supersede.txt", "", false},
		{"relative path reads against the cwd", "bd close gt-abc --reason-file supersede.txt", dir, false},
		{"a file without an override", "bd close gt-abc --reason-file " + dir + "/plain.txt", "", true},
		{"a missing file stays refused", "bd close gt-abc --reason-file " + dir + "/missing.txt", "", true},
		{"stdin cannot be read and stays refused", "bd close gt-abc --reason-file -", "", true},
		{"an explicit reason wins", "bd close gt-abc --reason 'cancel: x' --reason-file " + dir + "/plain.txt", "", false},
	}
	for _, tt := range tests {
		id, refusal := firstBdCloseRefusal(scope, parseBdCloseInvocations(tt.command), tt.cwd)
		if (refusal != "") != tt.refused {
			t.Errorf("%s: refused = %v (%q %q), want %v", tt.name, refusal != "", id, refusal, tt.refused)
		}
	}
}

// TestBdCloseInvariantBlockPath_NewForms drives each close form the parser
// learned in gt-0e0nz through the decision the guard turns into exit 2
// (firstBdCloseRefusal): parsing a form is not enough if the ids or the reason
// it read never reach the refusal. Every row closes gt-arno on a branch cut
// for gt-arno with unmerged commits, so a form the guard cannot see is allowed
// where it must be refused.
func TestBdCloseInvariantBlockPath_NewForms(t *testing.T) {
	t.Parallel()
	scope := bdCloseInvariantScope{
		branch:  "polecat/malachite/gt-arno+muck73gu",
		target:  "origin/main",
		counter: fakeCloseTimeCommitCounter{count: 3},
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/supersede.txt", []byte("supersede: folded into gt-y\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, command string
		wantID        string // the id the guard refuses; "" when the close passes
	}{
		{"dollar-paren substitution", "echo $(bd close gt-arno)", "gt-arno"},
		{"backtick substitution", "echo `bd close gt-arno`", "gt-arno"},
		{"eval", "eval 'bd close gt-arno'", "gt-arno"},
		{"xargs fed by echo", "echo gt-arno | xargs bd close", "gt-arno"},
		{"xargs fed by printf", `printf 'gt-arno\n' | xargs -n 1 bd close`, "gt-arno"},
		{"xargs with an unreadable producer", "cat ids.txt | xargs bd close", "gt-arno"},
		{"xargs with an unreadable producer and a lookup", "bd list --json | xargs bd close", "gt-arno"},
		{"xargs with no producer", "xargs bd close < ids.txt", "gt-arno"},
		{"env with flags", "env -u FOO bd close gt-arno", "gt-arno"},
		{"repeated reason leaves this id unoverridden", "bd close gt-arno gt-other --reason x --reason 'supersede: y'", "gt-arno"},
		{"reason file without an override", "bd close gt-arno --reason-file " + dir + "/missing.txt", "gt-arno"},

		{"substitution with a supersede reason", "echo $(bd close gt-arno --reason 'supersede: dup')", ""},
		{"xargs fed by echo with a supersede reason", "echo gt-arno | xargs bd close --reason 'cancel: x'", ""},
		{"xargs with an unreadable producer and a supersede reason", "cat ids.txt | xargs bd close --reason 'supersede: y'", ""},
		{"repeated reason overrides this id", "bd close gt-other gt-arno --reason x --reason 'supersede: y'", ""},
		{"reason file with an override", "bd close gt-arno --reason-file " + dir + "/supersede.txt", ""},
		{"a close of another bead through a new form", "echo gt-other | xargs bd close", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, refusal := firstBdCloseRefusal(scope, parseBdCloseInvocations(tt.command), "")
			if tt.wantID == "" {
				if refusal != "" {
					t.Errorf("%q refused %q (%s), want it allowed", tt.command, id, refusal)
				}
				return
			}
			if id != tt.wantID || refusal == "" {
				t.Errorf("%q refused (%q, %q), want %s refused", tt.command, id, refusal, tt.wantID)
			}
		})
	}

	t.Run("an unreadable producer on a branch that names no bead is allowed", func(t *testing.T) {
		t.Parallel()
		other := scope
		other.branch = "feature/cleanup"
		if id, refusal := firstBdCloseRefusal(other, parseBdCloseInvocations("cat ids.txt | xargs bd close"), ""); refusal != "" {
			t.Errorf("a branch cut for no bead has nothing to judge, but %q was refused: %s", id, refusal)
		}
	})
}

// TestBranchBeadID: the bead a polecat branch was cut for, and nothing for any
// other shape.
func TestBranchBeadID(t *testing.T) {
	t.Parallel()
	for branch, want := range map[string]string{
		"polecat/malachite/gt-arno+muck73gu": "gt-arno",
		"polecat/malachite/GT-Arno+muck73gu": "gt-arno",
		"polecat/malachite/gt-arno":          "gt-arno",
		"polecat/malachite/temp+muck73gu":    "",
		"feature/gt-arno":                    "",
		"main":                               "",
		"":                                   "",
	} {
		if got := branchBeadID(branch); got != want {
			t.Errorf("branchBeadID(%q) = %q, want %q", branch, got, want)
		}
	}
}

// TestReadBdCloseReasonFile_Bounded: the path comes from the command, so the
// read must not block on a FIFO, follow a device, or load an oversized file.
func TestReadBdCloseReasonFile_Bounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("a device is not read", func(t *testing.T) {
		t.Parallel()
		if got, ok := readBdCloseReasonFile("/dev/zero"); ok {
			t.Errorf("read %d bytes from /dev/zero, want it refused unread", len(got))
		}
	})

	t.Run("a directory is not read", func(t *testing.T) {
		t.Parallel()
		if _, ok := readBdCloseReasonFile(dir); ok {
			t.Error("read a directory, want it refused")
		}
	})

	t.Run("an oversized file is cut at the cap", func(t *testing.T) {
		t.Parallel()
		big := dir + "/big.txt"
		body := "supersede: " + strings.Repeat("x", 2*bdCloseReasonFileMaxBytes)
		if err := os.WriteFile(big, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := readBdCloseReasonFile(big)
		if !ok {
			t.Fatal("a regular file must be read")
		}
		if len(got) != bdCloseReasonFileMaxBytes {
			t.Errorf("read %d bytes, want the first %d", len(got), bdCloseReasonFileMaxBytes)
		}
		if !strings.HasPrefix(got, "supersede: ") {
			t.Errorf("the head of the file must survive the cut, got %.20q", got)
		}
	})

	t.Run("a regular file is trimmed", func(t *testing.T) {
		t.Parallel()
		p := dir + "/r.txt"
		if err := os.WriteFile(p, []byte("  cancel: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := readBdCloseReasonFile(p); !ok || got != "cancel: x" {
			t.Errorf("got (%q, %v), want (cancel: x, true)", got, ok)
		}
	})
}

// TestCloseInvariantBranchBead: the guard reads the bead its branch was cut
// for, so the target follows that bead's base_branch. A branch naming no bead
// skips the lookup, and a failed lookup yields nil so the target falls back to
// the rig default.
func TestCloseInvariantBranchBead(t *testing.T) {
	t.Parallel()
	const branch = "polecat/basalt/gt-6hmz+abc"
	bead := &beads.Issue{ID: "gt-6hmz", Description: `formula_vars: ["base_branch=develop"]`}

	t.Run("ReadsTheBranchBead", func(t *testing.T) {
		t.Parallel()
		var asked []string
		show := func(id string) (*beads.Issue, error) {
			asked = append(asked, id)
			return bead, nil
		}
		if got := closeInvariantBranchBead(branch, show); got != bead {
			t.Errorf("closeInvariantBranchBead = %v, want the branch's bead", got)
		}
		if !reflect.DeepEqual(asked, []string{"gt-6hmz"}) {
			t.Errorf("show called with %v, want [gt-6hmz]", asked)
		}
	})

	t.Run("NoBeadOnBranchSkipsLookup", func(t *testing.T) {
		t.Parallel()
		show := func(id string) (*beads.Issue, error) {
			t.Errorf("show called with %q for a branch naming no bead", id)
			return nil, nil
		}
		if got := closeInvariantBranchBead("main", show); got != nil {
			t.Errorf("closeInvariantBranchBead = %v, want nil", got)
		}
	})

	t.Run("LookupFailureFallsBackToNil", func(t *testing.T) {
		t.Parallel()
		show := func(id string) (*beads.Issue, error) {
			return bead, errors.New("bd show failed")
		}
		if got := closeInvariantBranchBead(branch, show); got != nil {
			t.Errorf("closeInvariantBranchBead = %v, want nil on lookup failure", got)
		}
	})
}

// bdCloseCoverageHazards are the commands the coverage property needs beyond the
// differential's own: a line-continued quoted id that parsedTokens reads as
// `$gt-x`, an ANSI-C quoted id, a substitution holding a blank, and a glued
// redirection with a dashed target.
var bdCloseCoverageHazards = []string{
	"bd close $\\\n'gt-x'",
	"bd close gt-x $\\\n'gt-y'",
	"bd close $'gt-x'",
	"bd close $(echo gt-x gt-y)",
	"bd close gt-x >gt-y",
}

// bdCloseCoverageCommands is the command source of TestParsedTokensDifferential
// (the guard corpus and the generated commands), each with a bd close placed at
// every position the differential's classes touch: after a separator, in a case
// arm, in a substitution, behind a glued redirection, and in an eval or sh -c
// payload. The hazard commands come first.
func bdCloseCoverageCommands(t *testing.T) []string {
	t.Helper()
	const cl = "bd close gt-x"
	commands := append([]string{}, bdCloseCoverageHazards...)
	for _, c := range parsedTokensCommands(t) {
		commands = append(commands,
			c,
			cl+" "+c,
			c+" ; "+cl,
			c+";"+cl,
			c+" && "+cl,
			c+" | "+cl,
			"echo $("+cl+") "+c,
			"echo $("+c+"; "+cl+")",
			"case a in a) "+cl+";; esac; "+c,
			"case a in a) "+c+" ;; b) "+cl+";; esac",
			c+" 2>f; "+cl,
			c+" >f;"+cl,
			"sh -c '"+cl+"'; "+c,
			"eval '"+cl+"'; "+c,
		)
	}
	return commands
}

// bdCloseShellReading is the shellTokenize reading, the reference the parsed
// reading is compared with.
func bdCloseShellReading(command string) ([]string, bool) {
	return shellTokenize(command), false
}

// bdCloseCovers reports whether invocation g keeps what the close w is judged by:
// every bead-shaped id of w, and the unreadable marker when w holds one. An id
// that is not bead-shaped is never compared, since the guard never judges one
// (ruling on 0c852cb47f2b).
func bdCloseCovers(g, w bdCloseInvocation) bool {
	if w.IDsUnreadable && !g.IDsUnreadable {
		return false
	}
	for _, id := range w.IDs {
		if isBeadIDShaped(id) && !slices.Contains(g.IDs, id) {
			return false
		}
	}
	return true
}

// bdCloseHasID reports whether an invocation in invocations carries id.
func bdCloseHasID(invocations []bdCloseInvocation, id string) bool {
	for _, inv := range invocations {
		if slices.Contains(inv.IDs, id) {
			return true
		}
	}
	return false
}

// bdCloseMissed returns "" when every invocation of want is covered by one of
// got. Otherwise it names what the first uncovered invocation lost: a bead-shaped
// id that no invocation of got holds, or "<unreadable close>" or "<close>" when
// the invocation carries no such id.
func bdCloseMissed(got, want []bdCloseInvocation) string {
	for _, w := range want {
		if slices.ContainsFunc(got, func(g bdCloseInvocation) bool { return bdCloseCovers(g, w) }) {
			continue
		}
		for _, id := range w.IDs {
			if isBeadIDShaped(id) && !bdCloseHasID(got, id) {
				return id
			}
		}
		if w.IDsUnreadable {
			return "<unreadable close>"
		}
		return "<close>"
	}
	return ""
}

// bdCloseLostInProduction is the production check: parseBdCloseInvocations, routing
// included, must cover every invocation the shell reading finds. It returns what
// was lost, or "".
func bdCloseLostInProduction(command string) string {
	return bdCloseMissed(parseBdCloseInvocations(command), parseBdCloseInvocationsWith(command, bdCloseShellReading))
}

// bdCloseParsedGap reads command with the parsed tokenizer alone and no fallback.
// It reports routed when some reading of command (the whole command, or a
// substitution or shell payload inside it) is one bdCloseReadTokens would send to
// the shell reading. Otherwise gap names what the parsed reading lost, or is "".
func bdCloseParsedGap(command string) (gap string, routed bool) {
	unsafe := false
	parsedOnly := func(c string) ([]string, bool) {
		tokens, readable := bdCloseParsedTokenizer(c)
		if !readable || bdCloseParsedUnsafe(tokens) {
			unsafe = true
			return nil, false
		}
		return tokens, true
	}
	got := parseBdCloseInvocationsWith(command, parsedOnly)
	if unsafe {
		return "", true
	}
	return bdCloseMissed(got, parseBdCloseInvocationsWith(command, bdCloseShellReading)), false
}

// useBdCloseParsedTokenizer puts read in place of the parsed tokenizer until t
// ends. Only a test that is not parallel may call it.
func useBdCloseParsedTokenizer(t *testing.T, read func(string) ([]string, bool)) {
	t.Helper()
	orig := bdCloseParsedTokenizer
	bdCloseParsedTokenizer = read
	t.Cleanup(func() { bdCloseParsedTokenizer = orig })
}

// bdCloseReadCounts counts the readings the production reader asked for.
type bdCloseReadCounts struct{ parsed, shell int }

// countBdCloseReadings counts the calls to both tokenizers until t ends. Only a
// test that is not parallel may call it.
func countBdCloseReadings(t *testing.T) *bdCloseReadCounts {
	t.Helper()
	counts := &bdCloseReadCounts{}
	parsed, shell := bdCloseParsedTokenizer, bdCloseShellTokenizer
	bdCloseParsedTokenizer = func(command string) ([]string, bool) {
		counts.parsed++
		return parsed(command)
	}
	bdCloseShellTokenizer = func(command string) []string {
		counts.shell++
		return shell(command)
	}
	t.Cleanup(func() { bdCloseParsedTokenizer, bdCloseShellTokenizer = parsed, shell })
	return counts
}

// TestParseBdCloseParsedReadingMatchesShellIDs is the coverage property, over the
// command source of TestParsedTokensDifferential with a bd close at each position:
// on every command none of whose readings is routed to the shell reading, the
// parsed reading loses no invocation and no bead-shaped id the shell reading finds.
func TestParseBdCloseParsedReadingMatchesShellIDs(t *testing.T) {
	t.Parallel()
	var parsedN, routedN int
	for _, command := range bdCloseCoverageCommands(t) {
		gap, routed := bdCloseParsedGap(command)
		if routed {
			routedN++
			continue
		}
		parsedN++
		if gap != "" {
			t.Errorf("parsed reading of %q loses %q that the shell reading finds", command, gap)
		}
	}
	if parsedN == 0 || routedN == 0 {
		t.Fatalf("the property ran on %d parsed and %d routed commands, want both", parsedN, routedN)
	}
}

// TestParseBdCloseCoverageCatchesDroppedSeparator is the mutation check: a parsed
// tokenizer that drops separators. Read on its own it loses closes, so the
// property sees the mutation. Through the production path the routing predicate
// reads the merged tokens as unsafe and sends those commands to the shell reading,
// so the guard loses nothing and the predicate routes more commands than the real
// tokenizer does. It is not parallel, because it swaps the package's parsed
// tokenizer.
func TestParseBdCloseCoverageCatchesDroppedSeparator(t *testing.T) {
	dropped := func(command string) ([]string, bool) {
		tokens, readable := parsedTokens(command)
		return slices.DeleteFunc(tokens, func(s string) bool { return shellCommandSeparators[s] }), readable
	}
	commands := bdCloseCoverageCommands(t)

	lost := 0
	for _, command := range commands {
		if gap := bdCloseMissed(parseBdCloseInvocationsWith(command, dropped), parseBdCloseInvocationsWith(command, bdCloseShellReading)); gap != "" {
			lost++
		}
	}
	if lost == 0 {
		t.Error("a parsed tokenizer that drops separators loses no close on any command, so the coverage property cannot see a dropped separator")
	}

	useBdCloseParsedTokenizer(t, dropped)
	for _, command := range commands {
		if gap := bdCloseLostInProduction(command); gap != "" {
			t.Errorf("parseBdCloseInvocations(%q) loses %q under a tokenizer that drops separators; the routing predicate must send it to the shell reading", command, gap)
		}
	}
	if real, mutant := bdCloseRoutedCount(commands, parsedTokens), bdCloseRoutedCount(commands, dropped); mutant <= real {
		t.Errorf("the routing predicate routes %d commands under a tokenizer that drops separators and %d with the real one, want more under the dropped separators", mutant, real)
	}
}

// bdCloseRoutedCount counts the commands whose top-level tokens from read the
// routing predicate sends to the shell reading.
func bdCloseRoutedCount(commands []string, read func(string) ([]string, bool)) int {
	routed := 0
	for _, command := range commands {
		if tokens, readable := read(stripHeredocBodies(strings.TrimSpace(command))); !readable || bdCloseParsedUnsafe(tokens) {
			routed++
		}
	}
	return routed
}

// bdCloseBoundedAlphabet is the alphabet of the bounded exhaustive test: the
// symbols the coverage hazards and the routing shapes are built from. ";;" is
// listed beside ";" because a lone ";" joined by single spaces can never make the
// case terminator the routing predicate exists for.
var bdCloseBoundedAlphabet = []string{
	"bd", "close", "gt-x", ";", ";;", "&&", "|", "(", ")", "{", "}",
	"case", "in", "esac", "a)", ">", "2>/dev/null", "$(", "`", `""`,
}

// bdCloseBoundedMaxSymbols is the longest command the bounded exhaustive test builds.
const bdCloseBoundedMaxSymbols = 4

// TestParseBdCloseBoundedExhaustiveNoLostClose builds every command of up to
// bdCloseBoundedMaxSymbols symbols from bdCloseBoundedAlphabet, joined by single
// spaces, and checks each one. The production reading must lose no invocation and
// no bead-shaped id the shell reading finds, and a command read by the parsed
// tokens alone must lose none either. The claim holds within this bound.
func TestParseBdCloseBoundedExhaustiveNoLostClose(t *testing.T) {
	t.Parallel()
	var parsedN, routedN atomic.Int64
	t.Cleanup(func() {
		if parsedN.Load() == 0 || routedN.Load() == 0 {
			t.Errorf("the bounded set ran on %d parsed and %d routed commands, want both", parsedN.Load(), routedN.Load())
		}
	})
	// One subtest per first symbol, so the 168420 commands run in parallel.
	for _, first := range bdCloseBoundedAlphabet {
		t.Run(first, func(t *testing.T) {
			t.Parallel()
			var failures []string
			checked := 0
			var walk func(parts []string)
			walk = func(parts []string) {
				command := strings.Join(parts, " ")
				checked++
				if lost := bdCloseLostInProduction(command); lost != "" {
					failures = append(failures, fmt.Sprintf("parseBdCloseInvocations(%q) loses %q", command, lost))
				}
				if gap, routed := bdCloseParsedGap(command); routed {
					routedN.Add(1)
				} else {
					parsedN.Add(1)
					if gap != "" {
						failures = append(failures, fmt.Sprintf("parsed reading of %q loses %q", command, gap))
					}
				}
				if len(parts) == bdCloseBoundedMaxSymbols {
					return
				}
				for _, s := range bdCloseBoundedAlphabet {
					walk(append(slices.Clone(parts), s))
				}
			}
			walk([]string{first})

			// The commands starting with first are the sums of len^k for k from 0 to
			// bdCloseBoundedMaxSymbols-1 further symbols.
			want, power := 0, 1
			for k := 1; k <= bdCloseBoundedMaxSymbols; k++ {
				want += power
				power *= len(bdCloseBoundedAlphabet)
			}
			if checked != want {
				t.Fatalf("checked %d commands, want %d", checked, want)
			}
			for i, f := range failures {
				if i == 10 {
					t.Errorf("%d more failures", len(failures)-10)
					break
				}
				t.Error(f)
			}
		})
	}
}

// TestParseBdCloseInvocationsMatchesShellIDs runs the production reader, routing
// included, over the coverage commands: it loses no invocation or bead-shaped id
// the shell reading finds on any of them.
func TestParseBdCloseInvocationsMatchesShellIDs(t *testing.T) {
	t.Parallel()
	for _, command := range bdCloseCoverageCommands(t) {
		if lost := bdCloseLostInProduction(command); lost != "" {
			t.Errorf("parseBdCloseInvocations(%q) loses %q that the shell reading finds", command, lost)
		}
	}
}

// TestParseBdCloseInvocationsRedirectTargetIsNotAnID: the target of a redirection
// beside a close is not one of its ids, as the shell reading has it.
func TestParseBdCloseInvocationsRedirectTargetIsNotAnID(t *testing.T) {
	t.Parallel()
	const command = "bd close gt-x >gt-y"
	got := parseBdCloseInvocations(command)
	if bdCloseHasID(got, "gt-y") {
		t.Errorf("parseBdCloseInvocations(%q) = %+v, want no gt-y: it is the redirection's target", command, got)
	}
	if !bdCloseHasID(got, "gt-x") {
		t.Errorf("parseBdCloseInvocations(%q) = %+v, want an invocation holding gt-x", command, got)
	}
}

// TestParseBdCloseInvocationsRoutesUnsafeShapes checks which reading the production
// reader takes. An unsafe shape goes to the shell reading, which finds the bead the
// parsed tokens alone lose; a plain close is read by parsedTokens alone. It is not
// parallel, because it counts the readings through the package's tokenizers.
func TestParseBdCloseInvocationsRoutesUnsafeShapes(t *testing.T) {
	tests := []struct {
		name    string
		command string
		routed  bool
	}{
		{"case arm terminator dropped by parsedTokens", "case a in a) make test ;; b) bd close gt-x;; esac", true},
		{"line-continued quoted id", "bd close $\\\n'gt-x'", true},
		{"command substitution with a blank", "bd close $(echo gt-x gt-y)", true},
		{"close beside a redirection", "bd close gt-x >gt-y", true},
		{"plain close", "bd close gt-x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts := countBdCloseReadings(t)
			got := parseBdCloseInvocations(tt.command)
			if !bdCloseHasID(got, "gt-x") {
				t.Errorf("parseBdCloseInvocations(%q) = %+v, want an invocation holding gt-x", tt.command, got)
			}
			if routed := counts.shell > 0; routed != tt.routed {
				t.Errorf("parseBdCloseInvocations(%q) read the shell reading %d times, want routed=%v", tt.command, counts.shell, tt.routed)
			}
		})
	}
}

// TestParseBdCloseInvocationsUnreadableFallsBackToShell: a command parsedTokens
// cannot read is read by shellTokenize, as the guard read every command before
// the move, so its closes are found and judged the same way.
func TestParseBdCloseInvocationsUnreadableFallsBackToShell(t *testing.T) {
	t.Parallel()
	command := "bd close gt-x && ("
	if _, readable := parsedTokens(command); readable {
		t.Fatalf("parsedTokens reads %q; the test needs a command it cannot read", command)
	}
	got := parseBdCloseInvocations(command)
	if want := parseBdCloseInvocationsWith(command, bdCloseShellReading); !reflect.DeepEqual(got, want) {
		t.Errorf("parseBdCloseInvocations(%q) = %+v, want the shell reading %+v", command, got, want)
	}
	if !bdCloseHasID(got, "gt-x") {
		t.Errorf("parseBdCloseInvocations(%q) = %+v, want an invocation holding gt-x", command, got)
	}
}

// TestBdClosePrefixedDashedName pins the `$` then dashed name shape the parsed
// reading routes to the shell reading. The letter test reads runes, so a multi-byte
// letter counts and a multi-byte symbol does not.
func TestBdClosePrefixedDashedName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tok  string
		want bool
	}{
		{"$gt-x", true},
		{"$_gt-x", true},
		{"$é-x", true},
		{"$€-x", false},
		{"$1-x", false},
		{"$-x", false},
		{"$gtx", false},
		{"gt-x", false},
		{"$", false},
	}
	for _, tt := range tests {
		if got := bdClosePrefixedDashedName(tt.tok); got != tt.want {
			t.Errorf("bdClosePrefixedDashedName(%q) = %v, want %v", tt.tok, got, tt.want)
		}
	}
}
