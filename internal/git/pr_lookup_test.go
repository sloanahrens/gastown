package git

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestLookupPullRequestRecordedURLSurvivesDeletedHead(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{reject: "baseRepository", rules: []ghRule{
		{prefix: []string{"pr", "view", "https://github.com/upstream/repo/pull/42"}, stdout: `{"number":42,"url":"https://github.com/upstream/repo/pull/42","state":"MERGED","mergedAt":"2026-07-13T12:00:00Z","headRefName":"fix/deleted-head","headRefOid":"abc123","headRepository":null,"headRepositoryOwner":{"login":"fork-owner"},"baseRepository":{"nameWithOwner":"upstream/repo"}}`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	pr, err := g.LookupPullRequest(PullRequestRef{URL: "https://github.com/upstream/repo/pull/42", Branch: "fix/deleted-head"})
	if err != nil {
		t.Fatalf("LookupPullRequest: %v", err)
	}
	if !pr.Merged() || pr.Number != 42 || pr.HeadOwner != "fork-owner" {
		t.Fatalf("unexpected PR: %+v", pr)
	}
	if pr.LookupSource != "recorded-url" {
		t.Fatalf("LookupSource = %q, want recorded-url", pr.LookupSource)
	}
}

func TestLookupPullRequestRecordedURLRejectsHeadDrift(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "view", "https://github.com/upstream/repo/pull/42"}, stdout: `{"number":42,"url":"https://github.com/upstream/repo/pull/42","state":"OPEN","mergedAt":"","headRefName":"fix/drift","headRefOid":"new-head","headRepository":{"nameWithOwner":"fork/repo"},"headRepositoryOwner":{"login":"fork"},"baseRepository":{"nameWithOwner":"upstream/repo"}}`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	_, err := g.LookupPullRequest(PullRequestRef{URL: "https://github.com/upstream/repo/pull/42", Branch: "fix/drift", HeadSHA: "submitted"})
	if err == nil || !strings.Contains(err.Error(), "head changed") {
		t.Fatalf("LookupPullRequest err = %v, want head changed", err)
	}
}

func TestLookupPullRequestQualifiedForkHead(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"api", "-X", "GET", "repos/upstream/repo/pulls"}, stdout: `[{"number":4474,"html_url":"https://github.com/upstream/repo/pull/4474","state":"open","merged_at":null,"head":{"ref":"fix/fork-head","sha":"sha4474","repo":{"full_name":"blairsilverberg/repo","owner":{"login":"blairsilverberg"}},"user":{"login":"blairsilverberg"}},"base":{"repo":{"full_name":"upstream/repo"}}}]`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	pr, err := g.LookupPullRequest(PullRequestRef{Branch: "fix/fork-head", HeadOwner: "blairsilverberg"})
	if err != nil {
		t.Fatalf("LookupPullRequest: %v", err)
	}
	if !pr.Open() || pr.Number != 4474 || pr.HeadOwner != "blairsilverberg" {
		t.Fatalf("unexpected PR: %+v", pr)
	}
	if pr.LookupSource != "qualified-head" {
		t.Fatalf("LookupSource = %q, want qualified-head", pr.LookupSource)
	}
}

func TestLookupPullRequestBranchAmbiguityFailsClosed(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "list"}, stdout: `[{"number":1,"url":"https://github.com/upstream/repo/pull/1","state":"OPEN","mergedAt":"","headRefName":"shared","headRefOid":"a1","headRepository":{"nameWithOwner":"one/repo"},"headRepositoryOwner":{"login":"one"},"baseRepository":{"nameWithOwner":"upstream/repo"}},{"number":2,"url":"https://github.com/upstream/repo/pull/2","state":"OPEN","mergedAt":"","headRefName":"shared","headRefOid":"b2","headRepository":{"nameWithOwner":"two/repo"},"headRepositoryOwner":{"login":"two"},"baseRepository":{"nameWithOwner":"upstream/repo"}}]`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	_, err := g.LookupPullRequest(PullRequestRef{Branch: "shared"})
	if !errors.Is(err, ErrPullRequestAmbiguous) {
		t.Fatalf("LookupPullRequest err = %v, want ErrPullRequestAmbiguous", err)
	}
	if !g.HasOpenPR("shared") {
		t.Fatal("HasOpenPR should protect branch deletion on ambiguous lookup")
	}
	state, protErr := g.PullRequestProtection(PullRequestRef{Branch: "shared"})
	if state != PRProtectionUnknown {
		t.Fatalf("PullRequestProtection state = %v, want PRProtectionUnknown for an ambiguous lookup", state)
	}
	if !errors.Is(protErr, ErrPullRequestAmbiguous) {
		t.Fatalf("PullRequestProtection err = %v, want ErrPullRequestAmbiguous", protErr)
	}
}

// TestPullRequestProtectionDistinguishesFailedLookupFromOpenPR is the
// regression for gt-ghpk: HasOpenPullRequest collapsed "the lookup failed" and
// "a PR is genuinely open" into the same true, so a caller reporting its
// verdict to an operator said "open PR exists" even when the branch has no
// GitHub remote at all. PullRequestProtection must keep the two apart.
func TestPullRequestProtectionDistinguishesFailedLookupFromOpenPR(t *testing.T) {
	t.Parallel()
	g := newTestGit(t, newScripted(map[string]reply{
		"remote get-url upstream": fail(2, "error: No such remote 'upstream'\n"),
		"remote get-url origin":   ok("/srv/git/local-origin.git\n"),
	}))

	state, err := g.PullRequestProtection(PullRequestRef{Branch: "some-branch"})
	if state != PRProtectionUnknown {
		t.Fatalf("PullRequestProtection state = %v, want PRProtectionUnknown for a non-GitHub remote", state)
	}
	if err == nil {
		t.Fatal("PullRequestProtection err = nil, want a lookup failure describing the non-GitHub remote")
	}
	if g.HasOpenPullRequest(PullRequestRef{Branch: "some-branch"}) != true {
		t.Fatal("HasOpenPullRequest should still fail closed (true) for a failed lookup")
	}
}

func TestPullRequestProtectionOpenPRIsOpen(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "list"}, stdout: `[{"number":7,"url":"https://github.com/upstream/repo/pull/7","state":"OPEN","mergedAt":"","headRefName":"feature","headRefOid":"abc","headRepository":{"nameWithOwner":"fork/repo"},"headRepositoryOwner":{"login":"fork"},"baseRepository":{"nameWithOwner":"upstream/repo"}}]`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	state, err := g.PullRequestProtection(PullRequestRef{Branch: "feature"})
	if err != nil {
		t.Fatalf("PullRequestProtection: %v", err)
	}
	if state != PRProtectionOpen {
		t.Fatalf("PullRequestProtection state = %v, want PRProtectionOpen", state)
	}
}

func TestPullRequestProtectionNoMatchIsNone(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "list"}, stdout: `[]`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	state, err := g.PullRequestProtection(PullRequestRef{Branch: "no-pr"})
	if err != nil {
		t.Fatalf("PullRequestProtection: %v", err)
	}
	if state != PRProtectionNone {
		t.Fatalf("PullRequestProtection state = %v, want PRProtectionNone", state)
	}
	if g.HasOpenPullRequest(PullRequestRef{Branch: "no-pr"}) {
		t.Fatal("HasOpenPullRequest should be false when the lookup completes and finds no PR")
	}
}

func TestLookupPullRequestBranchHeadSHADisambiguates(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "list"}, stdout: `[{"number":1,"url":"https://github.com/upstream/repo/pull/1","state":"CLOSED","mergedAt":"","headRefName":"shared","headRefOid":"old","headRepository":{"nameWithOwner":"one/repo"},"headRepositoryOwner":{"login":"one"},"baseRepository":{"nameWithOwner":"upstream/repo"}},{"number":2,"url":"https://github.com/upstream/repo/pull/2","state":"CLOSED","mergedAt":"2026-07-13T12:00:00Z","headRefName":"shared","headRefOid":"wanted","headRepository":{"nameWithOwner":"two/repo"},"headRepositoryOwner":{"login":"two"},"baseRepository":{"nameWithOwner":"upstream/repo"}}]`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	pr, err := g.LookupPullRequest(PullRequestRef{Branch: "shared", HeadSHA: "wanted"})
	if err != nil {
		t.Fatalf("LookupPullRequest: %v", err)
	}
	if pr.Number != 2 || !pr.Merged() {
		t.Fatalf("unexpected PR: %+v", pr)
	}
}

func TestFindPRNumberRequiresOpenPR(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "view", "99"}, stdout: `{"number":99,"url":"https://github.com/upstream/repo/pull/99","state":"CLOSED","mergedAt":"","headRefName":"closed","headRefOid":"abc","headRepository":{"nameWithOwner":"fork/repo"},"headRepositoryOwner":{"login":"fork"},"baseRepository":{"nameWithOwner":"upstream/repo"}}`},
	}}
	g := newTestGit(t, gitHubRemotes())
	g.gh = gh.run

	number, err := g.FindPRNumberForRef(PullRequestRef{Number: 99})
	if err != nil {
		t.Fatalf("FindPRNumberForRef: %v", err)
	}
	if number != 0 {
		t.Fatalf("FindPRNumberForRef = %d, want 0 for closed-unmerged PR", number)
	}
}

func TestPullRequestApprovalAndMergeUseResolvedURLAndRepo(t *testing.T) {
	t.Parallel()
	gh := &fakeGH{rules: []ghRule{
		{prefix: []string{"pr", "view"}, stdout: `{"reviewDecision":"APPROVED"}`},
		{prefix: []string{"pr", "merge"}},
	}}
	g := newTestGit(t, newScripted(nil))
	dir := g.workDir
	g.gh = gh.run
	pr := &PullRequestInfo{Number: 42, URL: "https://github.com/upstream/repo/pull/42", BaseRepo: "upstream/repo", HeadSHA: "abc123"}

	approved, err := g.IsPullRequestApproved(pr)
	if err != nil {
		t.Fatalf("IsPullRequestApproved: %v", err)
	}
	if !approved {
		t.Fatal("IsPullRequestApproved = false, want true")
	}
	if _, err := g.GhPrMergePullRequest(pr, "squash"); err != nil {
		t.Fatalf("GhPrMergePullRequest: %v", err)
	}

	want := []ghCall{
		{dir: dir, args: "pr view https://github.com/upstream/repo/pull/42 --json reviewDecision --repo upstream/repo"},
		{dir: dir, args: "pr merge https://github.com/upstream/repo/pull/42 --squash --match-head-commit abc123 --repo upstream/repo"},
	}
	if got := gh.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls = %v, want %v", got, want)
	}
}

// ghRule answers a gh invocation whose argv starts with prefix.
type ghRule struct {
	prefix []string
	stdout string
}

// ghCall is one recorded gh invocation: its working directory and its argv
// joined by spaces.
type ghCall struct {
	dir  string
	args string
}

// fakeGH stands in for the gh CLI behind Git's gh seam. It answers the first
// rule whose prefix matches, fails any argv containing reject the way gh
// refuses an unknown --json field, and fails everything else.
type fakeGH struct {
	rules  []ghRule
	reject string

	mu    sync.Mutex
	calls []ghCall
}

func (f *fakeGH) run(dir string, args ...string) (stdout, stderr []byte, err error) {
	joined := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, ghCall{dir: dir, args: joined})
	f.mu.Unlock()
	if f.reject != "" && strings.Contains(joined, f.reject) {
		return nil, []byte("unsupported field requested: " + joined + "\n"), ghExit(2)
	}
	for _, r := range f.rules {
		if len(args) >= len(r.prefix) && slices.Equal(args[:len(r.prefix)], r.prefix) {
			if r.stdout == "" {
				return nil, nil, nil
			}
			return []byte(r.stdout + "\n"), nil, nil
		}
	}
	return nil, []byte("unexpected gh args: " + joined + "\n"), ghExit(1)
}

func (f *fakeGH) all() []ghCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ghCall(nil), f.calls...)
}

// ghExit is a non-zero gh exit, matched like *exec.ExitError.
type ghExit int

func (e ghExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e ghExit) ExitCode() int { return int(e) }

// gitHubRemotes answers the remote lookups of a clone whose origin is the
// fork github.com/fork/repo and whose upstream is github.com/upstream/repo.
func gitHubRemotes() *scripted {
	return newScripted(map[string]reply{
		"remote get-url upstream": ok("https://github.com/upstream/repo.git\n"),
		"remote get-url origin":   ok("https://github.com/fork/repo.git\n"),
	})
}
