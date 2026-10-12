package cmd

import (
	"os"
	"reflect"
	"strings"
	"testing"
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
