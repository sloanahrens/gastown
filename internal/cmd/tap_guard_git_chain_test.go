package cmd

import "testing"

// A git matcher judges one invocation at a time. Once it had seen `git push`
// or `git reset` it kept reading every later token as that command's argument,
// across &&, ; and |, so a flag, branch or ref belonging to the NEXT command
// blocked a legitimate chain (gt-yapnr).

func TestPolecatMainPushIsJudgedPerInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		blocked bool
	}{
		{"git push -u origin polecat/x/gt-1 && git checkout main", false},
		{"git push origin polecat/x/gt-1; git merge main", false},
		{"git push origin polecat/x/gt-1 | tee main.log", false},
		{"git push origin HEAD:main", true},
		{"git status && git push origin HEAD:main", true},
		{"git push origin polecat/x/gt-1 && git push origin main", true},
		{"git -C . push origin HEAD:main", true},
		{"git push --all", true},
		{"git push origin polecat/x/gt-1", false},
	}
	for _, c := range cases {
		reason, _ := matchesPolecatMainPush(lowerTokens(c.command), true)
		if (reason != "") != c.blocked {
			t.Errorf("matchesPolecatMainPush(%q) blocked=%v, want %v", c.command, reason != "", c.blocked)
		}
	}
}

func TestGitResetRemoteRefIsJudgedPerInvocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		blocked bool
	}{
		{"git reset --soft HEAD~2 && git rebase origin/main", false},
		{"git reset HEAD f; git diff origin/main", false},
		{"git reset HEAD f | cat origin/main", false},
		{"git reset --hard origin/main", true},
		{"git status && git reset origin/main", true},
		{"git reset -- origin/main", false},
		{"git reset -- f && git reset origin/main", true},
		{"git -C repo reset --hard origin/main", true},
	}
	for _, c := range cases {
		reason, _ := matchesDangerousGitReset(lowerTokens(c.command))
		if (reason != "") != c.blocked {
			t.Errorf("matchesDangerousGitReset(%q) blocked=%v, want %v", c.command, reason != "", c.blocked)
		}
	}
}
