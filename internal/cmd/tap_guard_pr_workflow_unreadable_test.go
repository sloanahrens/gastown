package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/shellparse"
)

// TestPRWorkflowUnreadableVerdicts pins the pr-workflow verdict on each command
// shellparse cannot read. The guard refuses such a command when its text could
// spell a blocked command (prWorkflowSpelled) and allows it otherwise, so these
// rows hold that reading's verdicts; a change to either path must leave each
// row as it is (gt-mru91l).
func TestPRWorkflowUnreadableVerdicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"unterminated double quote after a gh pr create", `gh pr create --title "unterminated`, true},
		{"unterminated single quote after a branch creation", "git checkout -b feature/x 'unterminated", true},
		{"unterminated quote after a gh pr create on the line", `gh pr create --title "a" && echo 'oops`, true},
		{"unterminated quote after a switch -c", `git switch -c x "`, true},
		{"unterminated quote after a gh pr create behind an assignment", `GH_REPO=o/r gh pr create "`, true},
		{"unterminated quote after a gh pr create then a stray open", `gh pr create; echo $(`, true},
		{"unterminated quote in a commit message", `git commit -m "fix (x)`, false},
		{"unterminated quote after an ls", `ls -la "unterminated`, false},
		{"unterminated quote after a checkout with no creation flag", `git checkout main "unterminated`, false},
		{"unterminated substitution holding gh", "x=$(gh pr create --fill", true},
		{"unterminated backtick substitution holding gh", "x=`gh pr create --fill", true},
		{"unterminated substitution holding a backtick gh", "x=$(echo `gh pr create)", true},
		{"unterminated backtick body on two closing parens", "x=$(echo `gh pr create))", true},
		{"open substitution as the command after an echo", "echo $(echo `gh pr create)", true},
		{"unbalanced parenthesis after a gh pr create", "gh pr create )", true},
		{"unterminated if naming gh", "if true; then gh pr create", true},

		// Refused on the text they spell, not on what they run. Each names gh pr
		// create or a branch creation only inside an echo, a quote or a payload
		// the shell never runs, and the scanner allowed each one. The overseer
		// ruling (gt-mru91l, 11-08 23:36) accepts these flips, the same
		// fail-closed reading the bd-create-store guard gives an unreadable bd
		// create (gt-j41yim): a command the parser cannot read is not guessed at.
		// The echo of a quoted "gh" is the tenth such row: its letters spell gh
		// pr create just as g\h pr create would, and the shell runs that as gh.
		{"unterminated quote naming gh only in a quote", `echo "unterminated gh pr create`, true},
		{"unterminated single quote naming gh only in a quote", `echo 'gh pr create`, true},
		{"unterminated quote hiding a gh behind a semicolon", `echo "x; gh pr create`, true},
		{"unterminated quote after an echo that names gh", `echo hi; echo "gh pr create`, true},
		{"unterminated quote in a bash -c payload", "bash -c 'gh pr create", true},
		{"unterminated quote after an echo of a gh", `echo "gh" pr create "`, true},
		{"unterminated parameter expansion naming gh", "echo ${x gh pr create", true},
		{"unterminated ANSI-C quote naming gh", "echo $'gh pr create", true},
		{"unterminated arithmetic naming gh", "echo $((1+ gh pr create", true},
		{"unterminated substitution after an echo", `echo "$(date" gh pr create`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := shellparse.Parse(tt.command); err == nil {
				t.Fatalf("shellparse reads %q; the row is not an unreadable command", tt.command)
			}
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestLeadingBranchCreationUnreadable pins that a command shellparse cannot read
// never gets the polecat branch-creation exemption, even when its text starts
// with a branch creation (gt-mru91l).
func TestLeadingBranchCreationUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"unterminated quote after a leading checkout -b", "git checkout -b feature/x 'unterminated"},
		{"unterminated quote after a leading switch -c", `git switch -c x "`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := shellparse.Parse(tt.command); err == nil {
				t.Fatalf("shellparse reads %q; the row is not an unreadable command", tt.command)
			}
			if isLeadingBranchCreation(tt.command) {
				t.Errorf("isLeadingBranchCreation(%q) = true, want false", tt.command)
			}
		})
	}
}
