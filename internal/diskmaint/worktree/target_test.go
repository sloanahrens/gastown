package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// The shas the fake git reports. Each is a distinct 40-character value, so a
// Pushed check that compares against the wrong sha cannot pass by accident.
const (
	shaHead  = "1111111111111111111111111111111111111111"
	shaOther = "2222222222222222222222222222222222222222"
)

// fakeGit answers canned output per command line and records every call. A
// command no test answered fails the test, so each test names every git call
// the target may make and an unexpected one cannot pass unnoticed.
type fakeGit struct {
	t     *testing.T
	reply map[string]fakeReply
	calls []string
	envs  [][]string // the environment each command received
}

type fakeReply struct {
	out []byte
	err error
}

func newFakeGit(t *testing.T) *fakeGit {
	t.Helper()
	return &fakeGit{t: t, reply: map[string]fakeReply{}}
}

// answer sets what the command line returns. line is the program name and its
// arguments, e.g. "-C /town/rig-a/crew/a status --porcelain".
func (f *fakeGit) answer(line, out string, err error) {
	f.reply["git "+line] = fakeReply{out: []byte(out), err: err}
}

// answerErr sets the command line to fail, as a git that exited non-zero does.
func (f *fakeGit) answerErr(line string, err error) { f.answer(line, "", err) }

func (f *fakeGit) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	f.envs = append(f.envs, env)
	r, ok := f.reply[line]
	if !ok {
		f.t.Errorf("unexpected command: %s", line)
		return nil, errors.New("no canned reply")
	}
	return r.out, r.err
}

// callsMatching counts the recorded calls that contain part.
func (f *fakeGit) callsMatching(part string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, part) {
			n++
		}
	}
	return n
}

// townRoot is a temporary town root; every fixture directory lives under it.
func townRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// addCheckout makes <root>/<dir> a git checkout. Discovery reads a directory
// with a .git entry as a checkout, and a linked worktree's .git is a file, so
// nothing here needs a real clone.
func addCheckout(t *testing.T, root, dir string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(dir))
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("make checkout %s: %v", dir, err)
	}
	return path
}

// addPlainDir makes <root>/<dir> without a .git entry.
func addPlainDir(t *testing.T, root, dir string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(dir))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("make directory %s: %v", dir, err)
	}
	return path
}

// porcelainWorktree is one block of `git worktree list --porcelain`; an empty
// branch renders a detached worktree.
type porcelainWorktree struct{ path, head, branch string }

// listing renders `git worktree list --porcelain` output in the order git
// prints it.
func listing(wts ...porcelainWorktree) string {
	var b strings.Builder
	for _, w := range wts {
		b.WriteString("worktree " + w.path + "\n")
		if w.head != "" {
			b.WriteString("HEAD " + w.head + "\n")
		}
		if w.branch == "" {
			b.WriteString("detached\n")
		} else {
			b.WriteString("branch " + refsHeads + w.branch + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// settle answers git's reads for path as a clean worktree on branch, last
// committed at when, whose branch is on the remote at head and which no landing
// commit names. Each answer is keyed by git's exact command line, so a target
// that ran a different command would get no reply and fail the test.
func settle(f *fakeGit, path, branch, head string, when time.Time) {
	f.answer("-C "+path+" status --porcelain", "", nil)
	f.answer("-C "+path+" log -1 --format=%ct", strconv.FormatInt(when.Unix(), 10)+"\n", nil)
	f.answer("-C "+path+" ls-remote origin "+refsHeads+branch, head+"\t"+refsHeads+branch+"\n", nil)
	f.answer("-C "+path+" log origin/main --fixed-strings "+landedArg(branch)+" --format=%s", "", nil)
}

// noRedirect is a fixed environment with no repository-redirecting variable, so
// a test does not depend on the environment of the host it runs on.
func noRedirect() []string { return []string{"PATH=/usr/bin"} }

// landedArg is the --grep argument that narrows the walk to a branch's landing
// commit. The landing subject is "land: <branch> (<sha>) onto main (...)", and
// the subject check that follows the walk is what decides.
func landedArg(branch string) string { return "--grep=land: " + branch + " (" }

// planOne plans the town root with one rig and returns the plan.
func planOne(t *testing.T, root string, f *fakeGit, now time.Time) diskmaint.Plan {
	t.Helper()
	p, err := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect}).Plan(context.Background(), diskmaint.Policy{Now: now})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

func TestTargetIsATarget(t *testing.T) {
	t.Parallel()
	var target diskmaint.Target = NewTarget(Config{})
	if got := target.Name(); got != "worktree" {
		t.Fatalf("Name() = %q, want %q", got, "worktree")
	}
}

// Plan feeds BuildPlan the facts it read from git: a clean, pushed overseer-*
// worktree idle for 3 days goes, and a dirty one, one whose branch the remote
// does not hold at HEAD, and one that is not an overseer-* directory stay
// report-only.
func TestTargetPlanFromCannedGit(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	b := addCheckout(t, root, "rig-a/crew/overseer-b")
	c := addCheckout(t, root, "rig-a/crew/overseer-c")
	d := addCheckout(t, root, "rig-a/crew/other-d")
	const (
		branchA = "polecat/overseer-a/x"
		branchB = "polecat/overseer-b/x"
		branchC = "polecat/overseer-c/x"
		branchD = "polecat/other-d/x"
	)
	all := listing(
		porcelainWorktree{a, shaHead, branchA},
		porcelainWorktree{b, shaHead, branchB},
		porcelainWorktree{c, shaHead, branchC},
		porcelainWorktree{d, shaHead, branchD},
	)
	f := newFakeGit(t)
	for _, dir := range []string{a, b, c, d} {
		f.answer("-C "+dir+" worktree list --porcelain", all, nil)
	}
	old := testNow.Add(-72 * time.Hour)
	settle(f, a, branchA, shaHead, old)
	settle(f, b, branchB, shaHead, old)
	f.answer("-C "+b+" status --porcelain", " M one.go\n?? two.go\n", nil)
	settle(f, c, branchC, shaOther, old)
	settle(f, d, branchD, shaHead, old)

	p := planOne(t, root, f, testNow)
	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	if len(p.Actions) != 1 || p.Actions[0].Kind != "remove-worktree" || p.Actions[0].Ref != a {
		t.Fatalf("actions = %+v, want one remove-worktree on the clean pushed worktree %s", p.Actions, a)
	}
	if len(p.Kept) != 3 {
		t.Fatalf("kept = %+v, want b, c and d", p.Kept)
	}
	for _, k := range p.Kept {
		if !strings.HasPrefix(k.Rule, "report-only") {
			t.Fatalf("kept %+v, want a report-only rule for every worktree the plan leaves", k)
		}
	}
	var sawDirty bool
	for _, k := range p.Kept {
		if k.Ref != b {
			continue
		}
		sawDirty = true
		if want := "dirty 2"; !strings.Contains(k.Rule, want) {
			t.Fatalf("kept %+v, want %s kept for %q", k, b, want)
		}
	}
	if !sawDirty {
		t.Fatalf("kept = %+v, want %s, the dirty worktree, among them", p.Kept, b)
	}
}

// The dirty count the plan judges is the number of lines git status printed,
// not whether it printed anything.
func TestTargetPlanCountsEveryDirtyLine(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
	settle(f, a, "polecat/overseer-a/x", shaHead, testNow.Add(-72*time.Hour))
	f.answer("-C "+a+" status --porcelain", " M one.go\n?? two.go\n", nil)

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v, want none for a dirty worktree", p.Actions)
	}
	if len(p.Kept) != 1 || !strings.Contains(p.Kept[0].Rule, "dirty 2") {
		t.Fatalf("kept = %+v, want a rule naming 2 dirty entries", p.Kept)
	}
}

// A branch the remote does not hold at HEAD is not pushed, however recently it
// was committed.
func TestTargetPlanKeepsAWorktreeWhoseBranchIsNotPushed(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
	settle(f, a, "polecat/overseer-a/x", shaOther, testNow.Add(-72*time.Hour))

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 0 || len(p.Kept) != 1 {
		t.Fatalf("actions = %+v, kept = %+v, want the worktree kept", p.Actions, p.Kept)
	}
}

// A squash landing keeps no ancestry, so the landing commit is found by the
// branch name in its subject, never by asking whether the branch is an ancestor
// of origin/main. A landed worktree also loses its local branch.
func TestTargetLandedUsesGrepNotAncestry(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	const branch = "polecat/overseer-a/x"
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, branch}), nil)
	settle(f, a, branch, shaHead, testNow.Add(-72*time.Hour))
	f.answer("-C "+a+" ls-remote origin "+refsHeads+branch, "", nil)
	f.answer("-C "+a+" log origin/main --fixed-strings "+landedArg(branch)+" --format=%s", landingSubject(branch)+"\n", nil)

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 2 {
		t.Fatalf("actions = %+v, want remove-worktree then delete-branch", p.Actions)
	}
	if want := a + "#" + branch; p.Actions[1].Kind != "delete-branch" || p.Actions[1].Ref != want {
		t.Fatalf("second action = %+v, want delete-branch on %s", p.Actions[1], want)
	}
	want := "git -C " + a + " log origin/main --fixed-strings " + landedArg(branch) + " --format=%s"
	found := false
	for _, call := range f.calls {
		if call == want {
			found = true
		}
		if strings.Contains(call, "merge-base") || strings.Contains(call, "rev-list") {
			t.Fatalf("calls %v ask git for ancestry; a squash landing breaks it, so the branch name must be grepped", f.calls)
		}
	}
	if !found {
		t.Fatalf("calls %v do not include the landing grep %q", f.calls, want)
	}
}

// landingSubject is the subject the landing worker writes for branch, in the
// form "land: <branch> (<sha>) onto <target> (<sha>)".
func landingSubject(branch string) string {
	return "land: " + branch + " (" + shaHead[:8] + ") onto main (" + shaOther[:8] + ")"
}

// A commit that only quotes a landing is not the landing. A revert carries the
// landing it undoes in its own subject and in its body, and the walk --grep
// prints it; the subject check is what keeps it from marking unlanded work as
// landed, which would delete the branch of a worktree the remote never got.
func TestTargetLandedIgnoresACommitThatQuotesTheLanding(t *testing.T) {
	t.Parallel()
	const branch = "polecat/overseer-a/x"
	for name, quoted := range map[string]string{
		"a revert subject": `Revert "` + landingSubject(branch) + `"` + "\n",
		"another subject":  "docs: drop the stale landing note\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			a := addCheckout(t, root, "rig-a/crew/overseer-a")
			f := newFakeGit(t)
			f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, branch}), nil)
			settle(f, a, branch, shaHead, testNow.Add(-72*time.Hour))
			f.answer("-C "+a+" ls-remote origin "+refsHeads+branch, "", nil)
			f.answer("-C "+a+" log origin/main --fixed-strings "+landedArg(branch)+" --format=%s", quoted, nil)

			p := planOne(t, root, f, testNow)
			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none: %s is not a landing", p.Actions, strings.TrimSpace(quoted))
			}
			if len(p.Kept) != 1 || p.Kept[0].Ref != a {
				t.Fatalf("kept = %+v, want %s kept report-only", p.Kept, a)
			}
		})
	}
}

// A landing that the walk found but whose own landing is older than the quoting
// commit still counts: the walk returns every matching subject, not only the
// newest one, so an older real landing is not hidden by a later quote.
func TestTargetLandedFindsALandingOlderThanAQuote(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	const branch = "polecat/overseer-a/x"
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, branch}), nil)
	settle(f, a, branch, shaHead, testNow.Add(-72*time.Hour))
	f.answer("-C "+a+" ls-remote origin "+refsHeads+branch, "", nil)
	newestFirst := `Revert "` + landingSubject(branch) + `"` + "\n" + landingSubject(branch) + "\n"
	f.answer("-C "+a+" log origin/main --fixed-strings "+landedArg(branch)+" --format=%s", newestFirst, nil)

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 2 || p.Actions[1].Kind != "delete-branch" {
		t.Fatalf("actions = %+v, want remove-worktree then delete-branch for the landing under the revert", p.Actions)
	}
}

// A failed remote lookup and a failed landing search each leave the worktree
// unreadable, so the plan keeps it and says why; neither is read as "not
// pushed" or "not landed", which would judge the worktree on a fact it never got.
func TestTargetPlanRefusesWorktreeReadFailures(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		line string
		want error
	}{
		"a failed ls-remote":    {"ls-remote origin " + refsHeads + "polecat/overseer-a/x", errPushed},
		"a failed landing walk": {"log origin/main --fixed-strings " + landedArg("polecat/overseer-a/x") + " --format=%s", errLanded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			a := addCheckout(t, root, "rig-a/crew/overseer-a")
			const branch = "polecat/overseer-a/x"
			f := newFakeGit(t)
			f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, branch}), nil)
			settle(f, a, branch, shaHead, testNow.Add(-72*time.Hour))
			f.answerErr("-C "+a+" "+c.line, errors.New("exit status 128"))

			p := planOne(t, root, f, testNow)
			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none for a worktree whose read failed", p.Actions)
			}
			if len(p.Kept) != 1 || p.Kept[0].Ref != a {
				t.Fatalf("kept = %+v, want %s kept with its read error", p.Kept, a)
			}
			if !strings.Contains(p.Kept[0].Rule, c.want.Error()) {
				t.Fatalf("kept rule = %q, want the read error %q in the plan", p.Kept[0].Rule, c.want.Error())
			}
		})
	}
}

// The age gate is measured from the policy's clock, not the system's. The clock
// is years away from the run so a target that read the real time would see both
// commits in the future and keep both.
func TestTargetPlanJudgesAgeFromThePolicyClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		offset  time.Duration
		removed bool
	}{
		"idle 72 hours": {72 * time.Hour, true},
		"idle one hour": {time.Hour, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			a := addCheckout(t, root, "rig-a/crew/overseer-a")
			f := newFakeGit(t)
			f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
			settle(f, a, "polecat/overseer-a/x", shaHead, now.Add(-c.offset))

			p := planOne(t, root, f, now)
			if got := len(p.Actions) > 0; got != c.removed {
				t.Fatalf("actions = %+v, want removed=%v for a commit %v before the policy's now", p.Actions, c.removed, c.offset)
			}
		})
	}
}

// One worktree whose reads fail is kept with its error; the others are still
// planned, so one broken checkout does not stop the run.
func TestTargetPlanRefusesUnreadable(t *testing.T) {
	t.Parallel()

	// A listing that cannot be read could hide a worktree that must stay, so it
	// refuses the whole plan and names the clone it failed on.
	t.Run("a failed worktree list refuses the plan and names the clone", func(t *testing.T) {
		t.Parallel()
		root := townRoot(t)
		a := addCheckout(t, root, "rig-a/crew/overseer-a")
		f := newFakeGit(t)
		f.answerErr("-C "+a+" worktree list --porcelain", errors.New("exit status 128"))

		p := planOne(t, root, f, testNow)
		if !strings.Contains(p.Refused, a) || !strings.Contains(p.Refused, "git worktree list failed") {
			t.Fatalf("Refused = %q, want it to name %s and the failed listing", p.Refused, a)
		}
		if len(p.Actions) != 0 || len(p.Kept) != 0 {
			t.Fatalf("refused plan has actions %+v and kept %+v, want neither", p.Actions, p.Kept)
		}
	})

	// A worktree whose own read fails is kept with that error and read no
	// further; the other worktrees are still planned.
	t.Run("a failed read keeps that worktree and stops reading it", func(t *testing.T) {
		t.Parallel()
		root := townRoot(t)
		a := addCheckout(t, root, "rig-a/crew/overseer-a")
		b := addCheckout(t, root, "rig-a/crew/overseer-b")
		f := newFakeGit(t)
		f.answer("-C "+a+" worktree list --porcelain",
			listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}, porcelainWorktree{b, shaHead, "polecat/overseer-b/x"}), nil)
		f.answer("-C "+b+" worktree list --porcelain", listing(porcelainWorktree{b, shaHead, "polecat/overseer-b/x"}), nil)
		f.answerErr("-C "+a+" status --porcelain", errors.New("exit status 128"))
		settle(f, b, "polecat/overseer-b/x", shaHead, testNow.Add(-72*time.Hour))

		p := planOne(t, root, f, testNow)
		if len(p.Actions) != 1 || p.Actions[0].Ref != b {
			t.Fatalf("actions = %+v, want only %s removed", p.Actions, b)
		}
		if len(p.Kept) != 1 || p.Kept[0].Ref != a {
			t.Fatalf("kept = %+v, want %s kept with its read error", p.Kept, a)
		}
		if !strings.Contains(p.Kept[0].Rule, errStatus.Error()) {
			t.Fatalf("kept rule = %q, want the read error %q in the plan", p.Kept[0].Rule, errStatus.Error())
		}
		if got := f.callsMatching("-C " + a + " "); got != 2 {
			t.Fatalf("calls for %s = %d (%v), want only its listing and its failed status", a, got, f.calls)
		}
	})
}

// A crew directory that exists but cannot be listed refuses the plan; it is not
// read as a rig with no worktrees.
func TestTargetPlanRefusesWhenTheCrewDirectoryIsUnreadable(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "rig-a"), 0o755); err != nil {
		t.Fatalf("make rig directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "rig-a", "crew"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("make crew file: %v", err)
	}
	f := newFakeGit(t)

	p := planOne(t, root, f, testNow)
	if !strings.Contains(p.Refused, "crew directory is unreadable") {
		t.Fatalf("Refused = %q, want the unreadable crew directory named", p.Refused)
	}
}

// Worktrees of one clone appear in each other's listings, so the plan carries
// one entry per path, not one per listing.
func TestTargetPlanMergesOverlappingListings(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	b := addCheckout(t, root, "rig-a/crew/overseer-b")
	clone := filepath.Join(root, "rig-a", "crew", "overseer")
	both := listing(
		porcelainWorktree{clone, shaHead, "main"},
		porcelainWorktree{a, shaHead, "polecat/overseer-a/x"},
		porcelainWorktree{b, shaHead, "polecat/overseer-b/x"},
	)
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", both, nil)
	f.answer("-C "+b+" worktree list --porcelain", both, nil)
	settle(f, clone, "main", shaHead, testNow.Add(-72*time.Hour))
	settle(f, a, "polecat/overseer-a/x", shaHead, testNow.Add(-72*time.Hour))
	settle(f, b, "polecat/overseer-b/x", shaHead, testNow.Add(-72*time.Hour))

	p := planOne(t, root, f, testNow)
	if got := len(p.Actions); got != 2 {
		t.Fatalf("actions = %+v, want one remove-worktree per path (2), not one per listing (4)", p.Actions)
	}
	if got := len(p.Kept); got != 1 || p.Kept[0].Ref != clone {
		t.Fatalf("kept = %+v, want the clone once", p.Kept)
	}
}

// Only a directory that holds a checkout is asked; a plain directory or a file
// under crew is not a worktree.
func TestTargetPlanSkipsDirectoriesWithoutACheckout(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	addPlainDir(t, root, "rig-a/crew/notes")
	if err := os.WriteFile(filepath.Join(root, "rig-a", "crew", "README.md"), []byte("notes"), 0o644); err != nil {
		t.Fatalf("make README: %v", err)
	}
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
	settle(f, a, "polecat/overseer-a/x", shaHead, testNow.Add(-72*time.Hour))

	planOne(t, root, f, testNow)
	if got := f.callsMatching("worktree list --porcelain"); got != 1 {
		t.Fatalf("worktree list calls = %d, want 1 (only the checkout): %v", got, f.calls)
	}
}

// A rig with no crew directory holds no worktrees; that is not an error, and the
// other rigs are still read.
func TestTargetPlanSkipsARigWithNoCrewDirectory(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
	settle(f, a, "polecat/overseer-a/x", shaHead, testNow.Add(-72*time.Hour))

	p, err := NewTarget(Config{Town: root, Rigs: []string{"rig-a", "rig-b"}, Run: f.run, Environ: noRedirect}).Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Refused != "" || len(p.Actions) != 1 {
		t.Fatalf("plan = %+v, want one action and no refusal", p)
	}
}

// A detached worktree has no branch to compare, so it is kept as report-only
// and its push is never looked up.
func TestTargetPlanKeepsADetachedWorktree(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, ""}), nil)
	f.answer("-C "+a+" status --porcelain", "", nil)
	f.answer("-C "+a+" log -1 --format=%ct", strconv.FormatInt(testNow.Add(-72*time.Hour).Unix(), 10)+"\n", nil)

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 0 || len(p.Kept) != 1 {
		t.Fatalf("actions = %+v, kept = %+v, want the detached worktree kept", p.Actions, p.Kept)
	}
	if got := f.callsMatching("ls-remote"); got != 0 {
		t.Fatalf("ls-remote calls = %d, want none without a branch: %v", got, f.calls)
	}
}

// A commit time that is not a Unix timestamp leaves the worktree unreadable
// rather than idle for zero seconds.
func TestTargetPlanKeepsAWorktreeWithAnUnreadableCommitTime(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
	f.answer("-C "+a+" status --porcelain", "", nil)
	f.answer("-C "+a+" log -1 --format=%ct", "not-a-time\n", nil)

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 0 || len(p.Kept) != 1 {
		t.Fatalf("actions = %+v, kept = %+v, want the worktree kept", p.Actions, p.Kept)
	}
}

// With no HEAD in the listing there is nothing to compare the remote sha to, so
// the worktree is kept rather than judged not pushed.
func TestTargetPlanKeepsAWorktreeWithNoHead(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, "", "polecat/overseer-a/x"}), nil)
	f.answer("-C "+a+" status --porcelain", "", nil)
	f.answer("-C "+a+" log -1 --format=%ct", strconv.FormatInt(testNow.Add(-72*time.Hour).Unix(), 10)+"\n", nil)

	p := planOne(t, root, f, testNow)
	if len(p.Actions) != 0 || len(p.Kept) != 1 {
		t.Fatalf("actions = %+v, kept = %+v, want the worktree kept", p.Actions, p.Kept)
	}
}

// unusableConfigs are the configurations discovery cannot read anything with.
// Plan refuses each; Measure has no plan to refuse with, so it errors.
func unusableConfigs(t *testing.T) map[string]Config {
	t.Helper()
	return map[string]Config{
		"no runner": {Town: "/town", Rigs: []string{"rig-a"}},
		"no rigs":   {Town: "/town", Run: newFakeGit(t).run},
	}
}

func TestTargetPlanRefusesWithoutARunnerOrRigs(t *testing.T) {
	t.Parallel()
	for name, cfg := range unusableConfigs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Now: testNow})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if p.Refused == "" {
				t.Fatal("plan not refused, want a refusal for an unusable configuration")
			}
			if len(p.Actions) != 0 || len(p.Kept) != 0 {
				t.Fatalf("refused plan has actions %+v and kept %+v, want neither", p.Actions, p.Kept)
			}
		})
	}
}

// Measure reports an unusable configuration as an error rather than measuring a
// town it cannot read.
func TestTargetMeasureFailsWithoutARunnerOrRigs(t *testing.T) {
	t.Parallel()
	for name, cfg := range unusableConfigs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewTarget(cfg).Measure(context.Background()); err == nil {
				t.Fatal("Measure succeeded on an unusable configuration, want an error")
			}
		})
	}
}

// A cancelled run stops before the per-worktree reads, so it surfaces the
// cancellation instead of reporting every worktree unreadable.
func TestTargetPlanStopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect}).Plan(ctx, diskmaint.Policy{Now: testNow})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Plan error = %v, want the context's cancellation", err)
	}
	if got := f.callsMatching("status --porcelain"); got != 0 {
		t.Fatalf("status calls = %d (%v), want none after the cancellation", got, f.calls)
	}
}

// A crew entry whose .git cannot be inspected leaves the worktree unknown, so
// the plan refuses instead of dropping it as "not a checkout". A symlink that
// points at itself fails the stat in a way that is not "does not exist".
func TestTargetPlanRefusesACheckoutItCannotInspect(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	crew := addPlainDir(t, root, "rig-a/crew")
	loop := filepath.Join(crew, "overseer-loop")
	if err := os.MkdirAll(loop, 0o755); err != nil {
		t.Fatalf("make the loop directory: %v", err)
	}
	if err := os.Symlink(filepath.Join(loop, ".git"), filepath.Join(loop, ".git")); err != nil {
		t.Fatalf("make the .git symlink loop: %v", err)
	}

	p := planOne(t, root, newFakeGit(t), testNow)
	if !strings.Contains(p.Refused, "cannot tell whether the directory holds a checkout") {
		t.Fatalf("Refused = %q, want the unreadable checkout named", p.Refused)
	}
}

func TestTargetMeasureCountsTheWorktrees(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	b := addCheckout(t, root, "rig-a/crew/overseer-b")
	both := listing(porcelainWorktree{a, shaHead, "polecat/overseer-a/x"}, porcelainWorktree{b, shaHead, "polecat/overseer-b/x"})
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", both, nil)
	f.answer("-C "+b+" worktree list --porcelain", both, nil)

	m, err := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect}).Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if !strings.Contains(m.Detail, "2 crew worktrees") {
		t.Fatalf("Detail = %q, want the two worktrees counted once each", m.Detail)
	}
	if m.Used != 0 || m.Capacity != 0 {
		t.Fatalf("Used = %d, Capacity = %d, want 0 and 0: the worktrees' bytes are not measured", m.Used, m.Capacity)
	}
}

// A measure over a listing that failed is an error, never a count that leaves
// the unreadable worktrees out.
func TestTargetMeasureFailsOnAFailedListing(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	f := newFakeGit(t)
	f.answerErr("-C "+a+" worktree list --porcelain", errors.New("exit status 128"))

	if _, err := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect}).Measure(context.Background()); err == nil {
		t.Fatal("Measure succeeded over a failed listing, want an error")
	}
}

// A repository-redirecting variable would make git read another repository than
// the crew checkout, so the plan refuses before any command runs. The refusal
// names the variable and never its value, which can be a path.
func TestTargetPlanRefusesARedirectedGit(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env  []string
		want string // the variable a refusal names; "" when the plan must not refuse
	}{
		"a git dir":                 {[]string{"PATH=/usr/bin", "GIT_DIR=/elsewhere/.git"}, "GIT_DIR"},
		"an index file":             {[]string{"GIT_INDEX_FILE=/elsewhere/index"}, "GIT_INDEX_FILE"},
		"a config override":         {[]string{"GIT_CONFIG_PARAMETERS=remote.origin.url=/elsewhere"}, "GIT_CONFIG_PARAMETERS"},
		"a global config file":      {[]string{"GIT_CONFIG_GLOBAL=/elsewhere/.gitconfig"}, "GIT_CONFIG_GLOBAL"},
		"an unrelated git variable": {[]string{"GIT_TERMINAL_PROMPT=0"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			f := newFakeGit(t)
			cfg := Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: func() []string { return c.env }}
			p, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Now: testNow})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if c.want == "" {
				if p.Refused != "" {
					t.Fatalf("Refused = %q, want no refusal for an unrelated variable", p.Refused)
				}
				return
			}
			if !strings.Contains(p.Refused, c.want) || !strings.Contains(p.Refused, errRedirected.Error()) {
				t.Fatalf("Refused = %q, want it to name %s", p.Refused, c.want)
			}
			if strings.Contains(p.Refused, "/elsewhere") {
				t.Fatalf("Refused = %q echoes the variable's value", p.Refused)
			}
			if len(f.calls) != 0 {
				t.Fatalf("calls %v, want none: git must not run in a redirected environment", f.calls)
			}
		})
	}
}
