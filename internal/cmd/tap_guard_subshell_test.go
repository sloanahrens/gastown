package cmd

import (
	"slices"
	"testing"
)

// TestDangerousCommandInsideSubshell pins gt-5eniu: a command inside a plain
// parenthesized subshell is checked the same way the bare command is. The
// opening parenthesis used to be glued to the subshell's first word ("(git"),
// so every matcher that finds its command word by name — matchesDangerousGitPush,
// matchesDangerousRmRf, matchesDangerousGitReset, matchesGitResetHard and the
// polecat main-push — read "(git"/"(rm" and looked straight past it, while the
// braced spelling `{ git push -f; }` was blocked. A group's closing parenthesis
// hid the same way from the other end: glued to the last word it turned
// "origin main" into "main)" and "-f" into "-f)".
func TestDangerousCommandInsideSubshell(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// The four forms the gt-kocid/gt-yapnr/gt-pb77k reviews found open.
		{"force push in a subshell", `(git push -f)`, true},
		{"long force push in a subshell", `(git push --force origin main)`, true},
		{"reset onto a remote ref in a subshell", `(git reset --hard origin/main)`, true},
		{"soft reset onto a remote ref in a subshell", `(git reset --soft origin/main)`, true},
		{"filesystem destruction in a subshell", `(rm -rf /)`, true},

		// Spaced and nested spellings of the same thing.
		{"spaced subshell", `( git push -f )`, true},
		{"subshell in a compound line", `echo hi && (rm -rf /)`, true},
		{"subshell opening with a chained command", `(cd /tmp && git push -f)`, true},
		{"subshell inside a brace group", `{ (git push -f); }`, true},

		// Nothing legitimate became blockable.
		{"echo in a subshell", `(echo hi)`, false},
		{"status in a subshell", `(git status)`, false},
		{"branch push in a subshell", `(git push origin feature)`, false},
		{"scoped rm in a subshell", `(rm -rf ./build)`, false},
		{"read in a subshell", `(cat go.mod)`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v",
					tt.command, reason != "", reason, tt.blocked)
			}
		})
	}
}

// TestPolecatMainPushInsideSubshell is the polecat half of gt-5eniu: a
// subshell hides neither the command word nor the refspec's destination, so
// a polecat's `(git push origin main)` is blocked by the main-push rule while
// its own branch push stays allowed.
func TestPolecatMainPushInsideSubshell(t *testing.T) {
	t.Parallel()
	sess := guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "gastown/polecats/flint"}, "")}
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"main push in a subshell", `(git push origin main)`, true},
		{"HEAD:main push in a subshell", `(git push origin HEAD:main)`, true},
		{"own branch push in a subshell", `(git push origin polecat/flint/gt-5eniu)`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, sess)
			if (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v",
					tt.command, reason != "", reason, tt.blocked)
			}
			if tt.blocked && reason != polecatMainPushReason {
				t.Errorf("evaluateDangerousCommand(%q) blocked by %q, want the polecat main-push rule %q",
					tt.command, reason, polecatMainPushReason)
			}
		})
	}
}

// TestSubshellCdDoesNotCarry pins the other side of the same rule: now that a
// subshell's "(" is a token of its own, the cd walk has to read the group as a
// child process whose change is not this shell's — the glued spelling once
// looked invisible only because "(cd" was not the word "cd", and the spaced
// spelling was misread as a carried change. A brace group keeps carrying, as
// it does today (gt-ajyw8).
func TestSubshellCdDoesNotCarry(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	inside := t.TempDir()
	proc := fakeGuardProcess(nil, outside)
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"glued subshell cd does not carry", "cd " + outside + " ; (cd " + inside + ") ; echo hi", outside},
		{"spaced subshell cd does not carry", "cd " + outside + " ; ( cd " + inside + " ) ; echo hi", outside},
		{"brace group cd still carries", "cd " + outside + " ; { cd " + inside + " ; } ; echo hi", inside},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := shellTokenize(tt.command)
			got, status := cdWalkRoot(proc, tokens, len(tokens), outside, map[string]string{})
			if status != cdWalkPlaced || got != tt.want {
				t.Errorf("cdWalkRoot(%q) = (%q, %v), want (%q, cdWalkPlaced)", tt.command, got, status, tt.want)
			}
		})
	}
}

// TestSubshellParensAreTokensOfTheirOwn pins the tokenizer rule behind the
// blocks above: an unquoted "(" that opens a shell word and the ")" that
// closes it are emitted spaced, so they arrive as their own tokens. A ")"
// with no group open — a `case` pattern's — is left glued to its pattern
// word, which is what trimShellKeywords reads, and a parenthesized group
// inside a command substitution stays inside the substitution's quoting
// scope (gt-n8ir) rather than being split out of it.
func TestSubshellParensAreTokensOfTheirOwn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"glued subshell splits", `(git push -f)`, []string{"(", "git", "push", "-f", ")"}},
		{"spaced subshell splits", `( rm -rf / )`, []string{"(", "rm", "-rf", "/", ")"}},
		{"case pattern paren stays glued", `case x in a) echo hi ;; esac`,
			[]string{"case", "x", "in", "a)", "echo", "hi", ";", ";", "esac"}},
		{"substitution keeps its body in one token", `echo $(git reset --hard)`,
			[]string{"echo", "$(git", "reset", "--hard)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellTokenize(tt.command)
			if !slices.Equal(got, tt.want) {
				t.Errorf("shellTokenize(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}
