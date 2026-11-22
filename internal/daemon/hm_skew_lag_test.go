package daemon

import (
	"strings"
	"testing"
	"time"
)

// TestSkewLagReadsCommitsAndAge: skewLag reads the commit count of rev-list
// --count installed..remote and the difference of the two committer times, so the
// hm-skew check can say how far behind the installed hm is in the planner's two
// measures (gt-c88j63).
func TestSkewLagReadsCommitsAndAge(t *testing.T) {
	t.Parallel()
	a := hmSkewBehindAnswers()
	a.lagCount = "17\n"
	a.lagTimes = map[string]string{hmSkewTestInstalled: "100\n", hmSkewTestRemote: "90100\n"}
	d, _, _ := hmSkewDaemon(t, a)

	commits, age, err := d.skewLag("/clone", hmSkewTestInstalled, hmSkewTestRemote)

	if err != nil || commits != 17 || age != 25*time.Hour {
		t.Errorf("skewLag = %d, %s, %v; want 17 commits, 25h and no error", commits, age, err)
	}
}

// TestSkewLagUnparsableIsAnError: a count or a time git printed that is not a
// number is an error naming the git call, never a zero reading; and a revision
// that is not a commit id never reaches git (gt-c88j63).
func TestSkewLagUnparsableIsAnError(t *testing.T) {
	t.Parallel()
	good := map[string]string{hmSkewTestInstalled: "100\n", hmSkewTestRemote: "90100\n"}
	for _, tc := range []struct {
		name      string
		count     string
		times     map[string]string
		installed string
		want      string
	}{
		{"an unparsable count", "many\n", good, hmSkewTestInstalled, "rev-list"},
		{"an empty count", "", good, hmSkewTestInstalled, "rev-list"},
		{"an unparsable installed time", "17\n", map[string]string{hmSkewTestInstalled: "yesterday\n", hmSkewTestRemote: "90100\n"}, hmSkewTestInstalled, "show"},
		{"an unparsable remote time", "17\n", map[string]string{hmSkewTestInstalled: "100\n", hmSkewTestRemote: "\n"}, hmSkewTestInstalled, "show"},
		{"a revision that is not a commit id", "17\n", good, "--upload-pack=x", "not a commit id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := hmSkewBehindAnswers()
			a.lagCount, a.lagTimes = tc.count, tc.times
			d, _, _ := hmSkewDaemon(t, a)

			commits, age, err := d.skewLag("/clone", tc.installed, hmSkewTestRemote)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("skewLag = %d, %s, %v; want an error naming %q", commits, age, err, tc.want)
			}
			if commits != 0 || age != 0 {
				t.Errorf("skewLag returned %d, %s beside its error, want zero values", commits, age)
			}
		})
	}
}

// TestSkewLagGitFailureIsAnError: a git call that fails (the rev-list or a
// committer-time read) is an error naming that call, never a zero (gt-c88j63).
func TestSkewLagGitFailureIsAnError(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"rev-list", "show"} {
		t.Run(call, func(t *testing.T) {
			t.Parallel()
			a := hmSkewBehindAnswers()
			a.lagCount = "17\n"
			a.lagTimes = map[string]string{hmSkewTestInstalled: "100\n", hmSkewTestRemote: "90100\n"}
			a.lagErr = call
			d, _, _ := hmSkewDaemon(t, a)

			commits, age, err := d.skewLag("/clone", hmSkewTestInstalled, hmSkewTestRemote)

			if err == nil || !strings.Contains(err.Error(), call) || commits != 0 || age != 0 {
				t.Errorf("skewLag = %d, %s, %v; want zero values and an error naming %q", commits, age, err, call)
			}
		})
	}
}
