package presubmit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The commands ReadCommits runs, written out rather than built from its own
// constants: a test that rebuilt them would pass with a flag missing.
const (
	logCmd  = "log -z --format=%H%x00%P%x00%B --end-of-options origin/main..HEAD"
	diffCmd = "diff-tree -r --no-commit-id --name-only -z --end-of-options %s"
	rootCmd = "diff-tree -r --no-commit-id --name-only -z --root --end-of-options %s"
)

// gitReply is one canned answer for the fake runner.
type gitReply struct {
	out string
	err error
}

// gitFake answers git commands by their argument line and records every one it
// was asked, so a test pins the exact command and the parse of its output
// together. It starts no process (docs/testing.md).
type gitFake struct {
	t     *testing.T
	reply map[string]gitReply
	calls []string
}

func newGitFake(t *testing.T, reply map[string]gitReply) *gitFake {
	t.Helper()
	return &gitFake{t: t, reply: reply}
}

func (f *gitFake) runner(args []string) ([]byte, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	r, ok := f.reply[line]
	if !ok {
		f.t.Errorf("unexpected git command %q", line)
		return nil, fmt.Errorf("no canned answer for %q", line)
	}
	if r.err != nil {
		return nil, r.err
	}
	return []byte(r.out), nil
}

// TestReadCommitsReadsACommitAndThePathsItChanges: the reader asks git log for
// the branch's commits and each commit's diff against its parent, and fills the
// commit from the answers (gt-b9j3o.6).
func TestReadCommitsReadsACommitAndThePathsItChanges(t *testing.T) {
	t.Parallel()
	f := newGitFake(t, map[string]gitReply{
		logCmd:                               {out: "sha1\x00parent0\x00fix x (gt-1)\x00"},
		fmt.Sprintf(diffCmd, "parent0 sha1"): {out: "go.mod\x00internal/x/x.go\x00"},
	})
	commits, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("ReadCommits: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("read %d commits, want 1", len(commits))
	}
	c := commits[0]
	if c.SHA != "sha1" || strings.Join(c.Parents, " ") != "parent0" || c.Message != "fix x (gt-1)" {
		t.Errorf("commit = %+v, want sha1 with parent0 and its message", c)
	}
	if got := strings.Join(c.Changed, ","); got != "go.mod,internal/x/x.go" {
		t.Errorf("Changed = %q, want the two paths", got)
	}
	if len(f.calls) != 2 || f.calls[0] != logCmd || f.calls[1] != fmt.Sprintf(diffCmd, "parent0 sha1") {
		t.Errorf("git calls = %q, want the log then its parent diff", f.calls)
	}
}

// TestReadCommitsJudgesAMergeAgainstEveryParent: a merge that only carries a
// parent's go.mod change introduces none of its own and passes, while a merge
// that leaves go.mod different from every parent is its own unexplained change
// and fails (gt-b9j3o.6, the merge-commit finding).
func TestReadCommitsJudgesAMergeAgainstEveryParent(t *testing.T) {
	t.Parallel()
	const mergeLog = "m1\x00p1 p2\x00Merge branch 'main' into work\x00"
	tests := []struct {
		name       string
		p1, p2     string
		wantJudged int
	}{
		{
			name:       "the merge carries parent one's change",
			p1:         "a.go\x00go.mod\x00",
			p2:         "a.go\x00",
			wantJudged: 0,
		},
		{
			name:       "the merge leaves go.mod different from both parents",
			p1:         "a.go\x00go.mod\x00",
			p2:         "a.go\x00go.mod\x00",
			wantJudged: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newGitFake(t, map[string]gitReply{
				logCmd:                        {out: mergeLog},
				fmt.Sprintf(diffCmd, "p1 m1"): {out: tt.p1},
				fmt.Sprintf(diffCmd, "p2 m1"): {out: tt.p2},
			})
			commits, err := ReadCommits(f.runner, "origin/main", "HEAD")
			if err != nil {
				t.Fatalf("ReadCommits: %v", err)
			}
			if len(f.calls) != 3 {
				t.Errorf("git calls = %q, want the log and one diff per parent", f.calls)
			}
			res := MissingRationales(commits)
			if res.Judged != tt.wantJudged {
				t.Errorf("Judged = %d, want %d", res.Judged, tt.wantJudged)
			}
			if want := tt.wantJudged == 1; want != (len(res.Missing) == 1) {
				t.Errorf("Missing = %v, want a finding: %v", res.Missing, want)
			}
		})
	}
}

// TestReadCommitsReadsARootCommit: a commit with no parent is diffed with
// --root, so the commit that first adds a go.mod is judged rather than read as
// changing nothing (gt-b9j3o.6).
func TestReadCommitsReadsARootCommit(t *testing.T) {
	t.Parallel()
	f := newGitFake(t, map[string]gitReply{
		logCmd:                       {out: "root\x00\x00add go.mod\x00"},
		fmt.Sprintf(rootCmd, "root"): {out: "go.mod\x00"},
	})
	commits, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("ReadCommits: %v", err)
	}
	if len(commits) != 1 || len(commits[0].Parents) != 0 {
		t.Fatalf("commits = %+v, want the root commit", commits)
	}
	if got := MissingRationales(commits); got.Judged != 1 {
		t.Errorf("Judged = %d, want the root commit's go.mod judged", got.Judged)
	}
}

// TestReadCommitsKeepsAPathGitWouldQuote: a module file under a non-ASCII
// directory is judged like any other; path quoting would hide its base name
// (gt-b9j3o.6, the quoted-path finding).
func TestReadCommitsKeepsAPathGitWouldQuote(t *testing.T) {
	t.Parallel()
	const dir = "vendor/ünïcode"
	f := newGitFake(t, map[string]gitReply{
		logCmd:                         {out: "sha1\x00p\x00bump (gt-1)\x00"},
		fmt.Sprintf(diffCmd, "p sha1"): {out: dir + "/go.mod\x00"},
	})
	commits, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("ReadCommits: %v", err)
	}
	res := MissingRationales(commits)
	if res.Judged != 1 || len(res.Missing) != 1 {
		t.Fatalf("result = %+v, want the quoted-looking path judged and failing", res)
	}
	if want := dir + "/go.mod"; res.Missing[0].Files[0] != want {
		t.Errorf("finding names %q, want %q", res.Missing[0].Files[0], want)
	}
}

// TestReadCommitsKeepsABaseRefAnOperand: a base that begins with a dash is a
// revision, not an option, so a branch cannot silence the check by naming its
// base (gt-b9j3o.6, the base-ref finding).
func TestReadCommitsKeepsABaseRefAnOperand(t *testing.T) {
	t.Parallel()
	const dashBase = "--output=/tmp/x"
	f := newGitFake(t, map[string]gitReply{
		"log -z --format=%H%x00%P%x00%B --end-of-options " + dashBase + "..HEAD": {out: ""},
	})
	if _, err := ReadCommits(f.runner, dashBase, "HEAD"); err != nil {
		t.Fatalf("ReadCommits: %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("git calls = %q, want the log alone", f.calls)
	}
}

// TestReadCommitsOnABranchWithNoCommits: an empty range reads as no commits,
// not as an error, so the command can report that it judged none (gt-b9j3o.6).
func TestReadCommitsOnABranchWithNoCommits(t *testing.T) {
	t.Parallel()
	f := newGitFake(t, map[string]gitReply{logCmd: {out: ""}})
	commits, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("ReadCommits: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("read %d commits from an empty range, want 0", len(commits))
	}
}

// TestReadCommitsRefusesAFailedLog: a log git refused fails the check instead
// of reading as a branch with nothing to judge, and no diff is attempted.
func TestReadCommitsRefusesAFailedLog(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	f := newGitFake(t, map[string]gitReply{logCmd: {err: boom}})
	_, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if !errors.Is(err, boom) {
		t.Fatalf("ReadCommits = %v, want the git error", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("git calls = %q, want the log alone", f.calls)
	}
}

// TestReadCommitsRefusesAFailedDiff: a diff git refused fails the check and
// names the commit, rather than leaving it with no paths to judge.
func TestReadCommitsRefusesAFailedDiff(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	f := newGitFake(t, map[string]gitReply{
		logCmd:                         {out: "sha1\x00p\x00bump (gt-1)\x00"},
		fmt.Sprintf(diffCmd, "p sha1"): {err: boom},
	})
	_, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if !errors.Is(err, boom) {
		t.Fatalf("ReadCommits = %v, want the git error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "sha1") {
		t.Errorf("error = %v, want it to name the commit", err)
	}
}

// TestReadCommitsRefusesAFailedRootDiff: a root commit whose diff git refused
// fails the check too, rather than passing as a commit that changes nothing.
func TestReadCommitsRefusesAFailedRootDiff(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	f := newGitFake(t, map[string]gitReply{
		logCmd:                       {out: "root\x00\x00add go.mod\x00"},
		fmt.Sprintf(rootCmd, "root"): {err: boom},
	})
	_, err := ReadCommits(f.runner, "origin/main", "HEAD")
	if !errors.Is(err, boom) {
		t.Fatalf("ReadCommits = %v, want the git error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("error = %v, want it to name the commit", err)
	}
}

// TestReadCommitsRefusesLogOutputItCannotGroup: unreadable log output fails the
// branch rather than being read as the commits before the bad record.
func TestReadCommitsRefusesLogOutputItCannotGroup(t *testing.T) {
	t.Parallel()
	f := newGitFake(t, map[string]gitReply{logCmd: {out: "sha1\x00parent\x00"}})
	if _, err := ReadCommits(f.runner, "origin/main", "HEAD"); err == nil {
		t.Error("ReadCommits of ungroupable log output = nil error, want a refusal")
	}
	if len(f.calls) != 1 {
		t.Errorf("git calls = %q, want the log alone", f.calls)
	}
}

// TestParseLogRefusesOutputItCannotGroup: output whose fields are not
// sha/parents/message triples is refused, so a changed git format fails the
// check instead of judging fewer commits than the branch holds.
func TestParseLogRefusesOutputItCannotGroup(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"a\x00b\x00", "a\x00b\x00c\x00d\x00"} {
		if _, err := parseLog([]byte(out)); err == nil {
			t.Errorf("parseLog(%q) = nil error, want a refusal", out)
		}
	}
}

// TestParseLogRefusesARecordWithNoSHA: a record whose sha is empty is refused,
// since a finding on it could not name a commit.
func TestParseLogRefusesARecordWithNoSHA(t *testing.T) {
	t.Parallel()
	if _, err := parseLog([]byte("\x00p\x00bump (gt-1)\x00")); err == nil {
		t.Error("parseLog of a record with no sha = nil error, want a refusal")
	}
}
