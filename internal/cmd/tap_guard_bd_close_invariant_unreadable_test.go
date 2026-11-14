package cmd

import "testing"

// bdCloseVerdictScope is a branch that holds four unmerged commits for gt-x, so
// a close of gt-x with no override reason is refused.
func bdCloseVerdictScope() bdCloseInvariantScope {
	return bdCloseInvariantScope{
		branch:  "polecat/warboy/gt-x+abc123",
		target:  "origin/main",
		counter: fakeCloseTimeCommitCounter{count: 4},
	}
}

// bdCloseRefuses reports whether the guard refuses command: the production
// reading of it is judged against the branch scope.
func bdCloseRefuses(command string) bool {
	return bdCloseRefusesWith(command, bdCloseReadTokens)
}

// bdCloseRefusesWith reports whether the guard refuses command under the reading
// read gives, judged against the branch scope.
func bdCloseRefusesWith(command string, read bdCloseReader) bool {
	_, refusal := firstBdCloseRefusal(bdCloseVerdictScope(), parseBdCloseInvocationsWith(command, read), "")
	return refusal != ""
}

// TestBdCloseReadableVerdictsUnchanged pins the guard's verdict on commands the
// parser reads. The table was recorded on the guard before the unreadable
// fallback moved, so a change to the readable path that loosens any row fails
// here (gt-k6z48o).
func TestBdCloseReadableVerdictsUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		refused bool
	}{
		{"plain close of the branch bead", "bd close gt-x", true},
		{"close of another bead", "bd close gt-y", false},
		{"close under a supersede reason", "bd close gt-x --reason='supersede: folded into gt-y'", false},
		{"close under a cancel reason", `bd close gt-x --reason="cancel: abandoned"`, false},
		{"close after a cd", "cd /tmp && bd close gt-x", true},
		{"close followed by an echo", "bd close gt-x && echo done", true},
		{"close piped to cat", "bd close gt-x | cat", true},
		{"close named only in single quotes", "echo 'bd close gt-x'", false},
		{"close in a command substitution", `echo "$(bd close gt-x)"`, true},
		{"close in a bash -c payload", "bash -c 'bd close gt-x'", true},
		{"close in a heredoc body", "cat <<EOF\nbd close gt-x\nEOF", false},
		{"close in a case arm the parsed reading drops", "case a in a) make test ;; b) bd close gt-x;; esac", true},
		{"close beside a redirection", "bd close gt-x >gt-y", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := bdCloseRefuses(tt.command); got != tt.refused {
				t.Errorf("guard verdict on %q: refused=%v, want %v", tt.command, got, tt.refused)
			}
		})
	}
}

// TestUnreadableCommandFailsClosedInBdCloseInvariant pins the bd-close verdict
// on a command shellparse cannot read. A command whose text spells a bd close
// is a possible close of the branch bead: it is judged as a close of that bead
// whatever ids it names and whatever reason its unreadable text seems to carry,
// so the branch (four unmerged commits) refuses it. A command that does not
// spell one is allowed, as before (gt-k6z48o).
func TestUnreadableCommandFailsClosedInBdCloseInvariant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		refused bool
	}{
		{"unterminated paren after a close of the branch bead", "bd close gt-x && (", true},
		{"unterminated quote naming a close in a quote", `echo "bd close gt-x`, true},
		{"unterminated paren after a close of another bead", "bd close gt-y && (", true},
		{"unterminated paren after a close under a supersede reason", `bd close gt-x --reason="supersede: done" && (`, true},
		{"unterminated quote naming a close of another bead", `echo "bd close gt-y`, true},
		{"unterminated quote with no close spelled", `echo "unterminated gt-x`, false},
		{"unterminated quote with no close spelled, no id", `echo "unterminated`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, readable := parsedTokens(tt.command); readable {
				t.Fatalf("parsedTokens reads %q; the row is not an unreadable command", tt.command)
			}
			if got := bdCloseRefuses(tt.command); got != tt.refused {
				t.Errorf("guard verdict on unreadable %q: refused=%v, want %v", tt.command, got, tt.refused)
			}
		})
	}
}
