package cmd

import (
	"errors"
	"os"
	"reflect"
	"strings"
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
		// still the command word rather than a label's tail. Every non-flag
		// token after `close` is an id, so the `)` and the redirect land here
		// beside the real one; only an id that names the branch is judged.
		{name: "subshell holding the close keeps it", command: "( bd close gt-arno ) 2>/dev/null", want: []bdCloseInvocation{{IDs: []string{"gt-arno", ")", "2>/dev/null"}}}},
		{name: "function body is not a case arm", command: "foo() { bd close gt-arno; }", want: nil},
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
	if reflect.ValueOf(p.getenv).Pointer() != reflect.ValueOf(os.Getenv).Pointer() {
		t.Error("realGuardProcess does not read the environment through os.Getenv")
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
