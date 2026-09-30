package checkpoint

import (
	"strings"
	"testing"
)

// The squash and inspect tests run against fakeRepo (fakegit_test.go), an
// in-memory repository per test, so they start no git and run in parallel.
// checkpoint_integration_test.go pins the same behaviour against real git.

// featureRepo returns a fake repository on branch feature, cut from main's
// initial commit, holding one commit per subject (each adds its own file).
func featureRepo(t *testing.T, subjects ...string) *fakeRepo {
	t.Helper()
	r := newFakeRepo(t)
	r.checkoutNew("feature")
	for i, s := range subjects {
		r.commit(s, string(rune('a'+i))+".go", "package x")
	}
	return r
}

// assertTreeHas fails unless HEAD's tree holds every path.
func assertTreeHas(t *testing.T, r *fakeRepo, paths ...string) {
	t.Helper()
	tree := r.commits[r.headCommit()].tree
	for _, p := range paths {
		if _, ok := tree[p]; !ok {
			t.Errorf("HEAD's tree lacks %s after squash (tree: %v)", p, tree)
		}
	}
}

func TestCountWIPCommits_NoWIP(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", "add feature B")

	count, err := countWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 WIP commits, got %d", count)
	}
}

func TestCountWIPCommits_AllWIP(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, WIPCommitPrefix)

	count, err := countWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 WIP commits, got %d", count)
	}
}

func TestCountWIPCommits_Mixed(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "real work", WIPCommitPrefix, "more real work")

	count, err := countWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 WIP commit, got %d", count)
	}
}

func TestCountWIPCommits_BaseRefMissing(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix)

	if _, err := countWIPCommits(r.run, fakeDir, "no-such-base"); err == nil {
		t.Fatal("expected an error for an unresolvable base ref")
	}
}

func TestSquashWIPCommits_NoWIP(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "real work")

	wipCount, err := squashWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 0 {
		t.Errorf("expected 0, got %d", wipCount)
	}
	if subjects := r.subjectsSince("main"); len(subjects) != 1 || subjects[0] != "real work" {
		t.Errorf("expected [real work], got %v", subjects)
	}
	if w := r.wrote(); len(w) != 0 {
		t.Errorf("history rewritten with nothing to squash: %v", w)
	}
}

func TestSquashWIPCommits_AllWIP(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, WIPCommitPrefix)

	wipCount, err := squashWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 2 {
		t.Errorf("expected 2, got %d", wipCount)
	}
	subjects := r.subjectsSince("main")
	if len(subjects) != 1 || subjects[0] != "squashed WIP checkpoint commits" {
		t.Errorf("expected one commit with the generic message, got %v", subjects)
	}
	assertTreeHas(t, r, "a.go", "b.go")
}

func TestSquashWIPCommits_Mixed(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "implement auth handler", WIPCommitPrefix, "add auth tests", WIPCommitPrefix)

	wipCount, err := squashWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 2 {
		t.Errorf("expected 2, got %d", wipCount)
	}
	// git log lists newest first, so the newest real subject is the title.
	subjects := r.subjectsSince("main")
	if len(subjects) != 1 || subjects[0] != "add auth tests" {
		t.Errorf("expected one commit titled by the newest real subject, got %v", subjects)
	}
	msg := r.commits[r.headCommit()].message
	if msg != "add auth tests\n\n- implement auth handler" {
		t.Errorf("message = %q, want the other real subject as a body bullet", msg)
	}
	if strings.Contains(msg, WIPCommitPrefix) {
		t.Errorf("message = %q, want no WIP subject carried over", msg)
	}
	assertTreeHas(t, r, "a.go", "b.go", "c.go", "d.go")
}

// The squash commit sits directly on the merge base, not on the old tip.
func TestSquashWIPCommits_ResetsToMergeBase(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, "real")
	base := r.refs["refs/heads/main"]

	if _, err := squashWIPCommits(r.run, fakeDir, "main"); err != nil {
		t.Fatal(err)
	}
	head := r.commits[r.headCommit()]
	if len(head.parents) != 1 || head.parents[0] != base {
		t.Errorf("squash parents = %v, want [%s]", head.parents, base)
	}
	want := [][]string{{"reset", "--soft", base}, {"commit", "-m", "real"}}
	if w := r.wrote(); len(w) != 2 || strings.Join(w[0], " ") != strings.Join(want[0], " ") || strings.Join(w[1], " ") != strings.Join(want[1], " ") {
		t.Errorf("writes = %q, want %q", w, want)
	}
}

func TestSquashWIPCommits_NoCommits(t *testing.T) {
	t.Parallel()
	r := featureRepo(t)

	wipCount, err := squashWIPCommits(r.run, fakeDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 0 {
		t.Errorf("expected 0 for no commits, got %d", wipCount)
	}
}

func TestIsAutoSaveSubject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		subject string
		want    bool
	}{
		{WIPCommitPrefix, true},
		{"WIP: checkpoint (auto) 2026-09-08", true},
		{"fix: auto-save uncommitted implementation work (gt-pvx safety net)", true},
		{"fix: auto-save uncommitted implementation work (gt-wov, gt-pvx safety net)", true},
		{"fix: real bug in auto-save handling", false},
		{"implement feature", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsAutoSaveSubject(c.subject); got != c.want {
			t.Errorf("IsAutoSaveSubject(%q) = %v, want %v", c.subject, got, c.want)
		}
	}
}

func TestSquashAutoSaveCommits_AllGenerated_UsesFallbackTitle(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, "fix: auto-save uncommitted implementation work (gt-abc, gt-pvx safety net)")

	count, err := squashAutoSaveCommits(r.run, fakeDir, "main", "fix: handle nil pointer in auth (gt-abc)")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 squashed, got %d", count)
	}
	subjects := r.subjectsSince("main")
	if len(subjects) != 1 || subjects[0] != "fix: handle nil pointer in auth (gt-abc)" {
		t.Fatalf("expected the fallback title as the only subject, got %v", subjects)
	}
	assertTreeHas(t, r, "a.go", "b.go")
}

func TestSquashAutoSaveCommits_AllGenerated_EmptyFallback(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "fix: auto-save uncommitted implementation work (gt-pvx safety net)")

	count, err := squashAutoSaveCommits(r.run, fakeDir, "main", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 squashed, got %d", count)
	}
	if subjects := r.subjectsSince("main"); len(subjects) != 1 || subjects[0] != "squashed auto-save checkpoint commits" {
		t.Errorf("expected generic subject, got %v", subjects)
	}
}

func TestSquashAutoSaveCommits_Mixed_KeepsRealSubject(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "implement auth handler (gt-abc)", WIPCommitPrefix, "fix: auto-save uncommitted implementation work (gt-pvx safety net)")

	count, err := squashAutoSaveCommits(r.run, fakeDir, "main", "fallback title")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 squashed, got %d", count)
	}
	subjects := r.subjectsSince("main")
	if len(subjects) != 1 || subjects[0] != "implement auth handler (gt-abc)" {
		t.Fatalf("expected the real subject preserved as title, got %v", subjects)
	}
	assertTreeHas(t, r, "a.go", "b.go", "c.go")
}

func TestSquashAutoSaveCommits_NoGenerated_Untouched(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", "add feature B")

	count, err := squashAutoSaveCommits(r.run, fakeDir, "main", "fallback title")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 squashed, got %d", count)
	}
	if subjects := r.subjectsSince("main"); len(subjects) != 2 {
		t.Errorf("expected history untouched (2 commits), got %v", subjects)
	}
}

// TestHasAutoSaveCommits_BaseRefMissing covers gt-c1mw: callers that treat a
// zero-value (false, nil-checked-away) return as "no auto-save commits" must
// see an actual error here, not a silent false.
func TestHasAutoSaveCommits_BaseRefMissing(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix)

	has, err := hasAutoSaveCommits(r.run, fakeDir, "no-such-base-ref", "feature")
	if err == nil {
		t.Fatal("expected an error when the base ref cannot be resolved, got nil")
	}
	if has {
		t.Error("expected false alongside the error")
	}
}

func TestHasAutoSaveCommits_None(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A")

	has, err := hasAutoSaveCommits(r.run, fakeDir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no auto-save commits")
	}
}

func TestHasAutoSaveCommits_WIPTip(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix)

	has, err := hasAutoSaveCommits(r.run, fakeDir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected an auto-save commit to be detected")
	}
}

func TestHasAutoSaveCommits_WIPNotTip(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, "fix: finish the feature")

	has, err := hasAutoSaveCommits(r.run, fakeDir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected an auto-save commit buried earlier in the range to be detected")
	}
}

// hasAutoSaveCommits is read-only: it must reach a ref other than the current
// branch without checking it out or rewriting anything.
func TestHasAutoSaveCommits_ReadsAnotherRefWithoutWriting(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, AutoSaveCommitPrefix)
	r.checkout("main")

	has, err := hasAutoSaveCommits(r.run, fakeDir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected the auto-save commit on feature to be seen from main")
	}
	if w := r.wrote(); len(w) != 0 {
		t.Errorf("hasAutoSaveCommits wrote to the repository: %v", w)
	}
	if r.head != "refs/heads/main" {
		t.Errorf("HEAD moved to %s", r.head)
	}
}

func TestInspectAutoSaveTip_RealTip(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", "fix: finish feature A")

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := AutoSaveTip{Subject: "fix: finish feature A", Ahead: 2}
	if tip != want {
		t.Errorf("tip = %+v, want %+v", tip, want)
	}
}

// The gt-iki6 shape: real work with a checkpoint_dog commit on top of it.
func TestInspectAutoSaveTip_WIPTip(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", WIPCommitPrefix)

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := AutoSaveTip{Subject: WIPCommitPrefix, AutoSave: true, Trailing: 1, Ahead: 2}
	if tip != want {
		t.Errorf("tip = %+v, want %+v", tip, want)
	}
}

func TestInspectAutoSaveTip_AllGenerated(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, AutoSaveCommitPrefix)

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !tip.AutoSave || tip.Trailing != 2 || tip.Ahead != 2 {
		t.Errorf("expected the whole branch to be machine-generated (2 of 2), got %+v", tip)
	}
	if tip.BeneathIsMerge {
		t.Errorf("BeneathIsMerge = true with nothing beneath the run")
	}
}

// A machine-generated commit buried under real work leaves the tip
// submittable, so Trailing counts only the run at the tip.
func TestInspectAutoSaveTip_WIPNotTip(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix, "fix: finish the feature")

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := AutoSaveTip{Subject: "fix: finish the feature", Ahead: 2}
	if tip != want {
		t.Errorf("tip = %+v, want %+v", tip, want)
	}
}

func TestInspectAutoSaveTip_NoCommits(t *testing.T) {
	t.Parallel()
	r := featureRepo(t)

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip != (AutoSaveTip{}) {
		t.Errorf("expected the zero value for a branch with no commits, got %+v", tip)
	}
}

func TestInspectAutoSaveTip_BaseRefMissing(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, WIPCommitPrefix)

	if _, err := inspectAutoSaveTip(r.run, fakeDir, "origin/main", "HEAD"); err == nil {
		t.Fatal("expected an error for an unresolvable base ref")
	}
}

// An empty tip subject must not read as the commit beneath it: naming the
// wrong commit makes the refusal's rewrite advice target the wrong HEAD~N.
func TestInspectAutoSaveTip_EmptyTipSubject(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", "")

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	want := AutoSaveTip{Subject: "", Ahead: 2}
	if tip != want {
		t.Errorf("tip = %+v, want %+v", tip, want)
	}
}

// The commit the run folds into decides whether an amend keeps a real message,
// so a merge there must be visible to the caller.
func TestInspectAutoSaveTip_MergeBeneathTheRun(t *testing.T) {
	t.Parallel()
	r := newFakeRepo(t)
	r.checkoutNew("side")
	r.commit("add side work", "side.go", "package side")
	r.checkout("main")
	r.checkoutNew("feature")
	r.merge("Merge branch 'side'", "side")
	r.commit(WIPCommitPrefix, "b.go", "package b")

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !tip.AutoSave || tip.Trailing != 1 {
		t.Fatalf("expected a single machine-generated tip, got %+v", tip)
	}
	if !tip.BeneathIsMerge {
		t.Errorf("expected the merge beneath the run to be reported, got %+v", tip)
	}
	var asked bool
	for _, c := range r.calls {
		asked = asked || strings.Join(c, " ") == "log -1 --format=%p HEAD~1"
	}
	if !asked {
		t.Errorf("calls = %q, want the parents of HEAD~1 read", r.calls)
	}
}

func TestInspectAutoSaveTip_RealCommitBeneathTheRun(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", WIPCommitPrefix)

	tip, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if tip.BeneathIsMerge {
		t.Errorf("expected a non-merge commit beneath the run, got %+v", tip)
	}
}

// inspectAutoSaveTip never checks anything out or rewrites history, so
// callers may run it on a branch they are about to submit.
func TestInspectAutoSaveTip_DoesNotWrite(t *testing.T) {
	t.Parallel()
	r := featureRepo(t, "add feature A", WIPCommitPrefix)

	if _, err := inspectAutoSaveTip(r.run, fakeDir, "main", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if w := r.wrote(); len(w) != 0 {
		t.Errorf("inspectAutoSaveTip wrote to the repository: %v", w)
	}
}

func TestParseSubjectRecords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"one", "tip\x1e", []string{"tip"}},
		{"empty tip keeps its slot", "\x1e\nbelow\x1e", []string{"", "below"}},
		{"trailing newline after the sentinel is not a record", "a\x1e\nb\x1e\n", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseSubjectRecords(tt.in)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") || len(got) != len(tt.want) {
				t.Errorf("parseSubjectRecords(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
