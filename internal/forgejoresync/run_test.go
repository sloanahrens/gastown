package forgejoresync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
)

// runWork is the scratch directory the fake makes, and runScratch the bare repository inside it.
const (
	runGitHub  = "https://github.com/acme/rig.git"
	runForgejo = "http://forgejo.test/acme/rig.git"
	runWork    = "/scratch"
	runScratch = runWork + "/refs.git"
)

// runGitHubRefs and runForgejoRefs are the two listings the run starts from, each in ref-name
// order: GitHub is ahead on main, Forgejo holds a branch GitHub dropped, and the tag matches.
var (
	runGitHubRefs = []Ref{
		{Name: "refs/heads/feature-a", SHA: "bbbb2222"},
		{Name: "refs/heads/main", SHA: "aaaa1111"},
		{Name: "refs/tags/v1.0.0", SHA: "cccc3333"},
	}
	runForgejoRefs = []Ref{
		{Name: "refs/heads/feature-a", SHA: "bbbb2222"},
		{Name: "refs/heads/main", SHA: "dddd4444"},
		{Name: "refs/heads/stale-branch", SHA: "eeee5555"},
		{Name: "refs/tags/v1.0.0", SHA: "cccc3333"},
	}
)

// fakeMirror stands in for git. A remote is a ref list keyed by its URL: a fetch copies a
// remote's refs into the scratch repository and a push copies the scratch refs onto the
// remote, so the mirror converges as the real one does. Each call is appended to events.
type fakeMirror struct {
	remotes map[string][]Ref
	scratch map[string][]Ref
	events  *[]string
	// failAt is the call that fails: "ls-remote <url>", "init", "fetch" or "push".
	failAt string
	// failAfterPush is an "ls-remote <url>" call that fails only once a push has run, so the
	// verify stage's listing is the one that fails.
	failAfterPush string
	pushed        bool
	// noPush makes a push send nothing, so the remote keeps its refs.
	noPush bool
	// changed counts the refs the pushes created, updated or deleted.
	changed int
}

// newFakeMirror returns a mirror that records into events.
func newFakeMirror(events *[]string) *fakeMirror {
	return &fakeMirror{remotes: map[string][]Ref{}, scratch: map[string][]Ref{}, events: events}
}

// record appends one call to the shared event log.
func (f *fakeMirror) record(event string) { *f.events = append(*f.events, event) }

// LsRemote lists the refs of url in the form git ls-remote prints them.
func (f *fakeMirror) LsRemote(url string) ([]byte, error) {
	f.record("ls-remote " + url)
	if f.failAt == "ls-remote "+url || (f.pushed && f.failAfterPush == "ls-remote "+url) {
		return nil, errors.New("ls-remote: exit status 128")
	}
	var b strings.Builder
	for _, ref := range f.remotes[url] {
		fmt.Fprintf(&b, "%s\t%s\n", ref.SHA, ref.Name)
	}
	return []byte(b.String()), nil
}

// InitBare makes an empty scratch repository at dir.
func (f *fakeMirror) InitBare(dir string) error {
	f.record("init " + dir)
	if f.failAt == "init" {
		return errors.New("init: exit status 128")
	}
	f.scratch[dir] = nil
	return nil
}

// Fetch copies the refs of url into the scratch repository at dir.
func (f *fakeMirror) Fetch(dir, url string, _ ...string) error {
	f.record("fetch " + url)
	if f.failAt == "fetch" {
		return errors.New("fetch: exit status 128")
	}
	f.scratch[dir] = slices.Clone(f.remotes[url])
	return nil
}

// PushPrune makes url's refs the scratch repository's refs, and counts what changed.
func (f *fakeMirror) PushPrune(dir, url string, refspecs ...string) error {
	f.record("push " + url + " " + strings.Join(refspecs, " "))
	if f.failAt == "push" {
		return errors.New("push: exit status 1")
	}
	f.pushed = true
	if f.noPush {
		return nil
	}
	push, del := Plan(f.scratch[dir], f.remotes[url])
	f.changed += len(push) + len(del)
	f.remotes[url] = slices.Clone(f.scratch[dir])
	return nil
}

// authRecorder records the Authorization header of each request before the transport beneath
// answers it.
type authRecorder struct {
	auths *[]string
	next  http.RoundTripper
}

// RoundTrip records req's Authorization header and passes the request on.
func (a authRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	*a.auths = append(*a.auths, req.Header.Get("Authorization"))
	return a.next.RoundTrip(req)
}

// runSetup is what one run test varies: the variable the admin token is read from, the other
// environment it sees, whether the repository has main's rule, and whether it is a dry run.
type runSetup struct {
	tokenVar string
	env      map[string]string
	noRule   bool
	dryRun   bool
}

// runFixture is one run wired to a fake mirror, the fake Forgejo behind the run's own client,
// and in-memory files and provisioner. events holds the order every seam saw; logs the progress
// lines; calls the provisioner's calls.
type runFixture struct {
	resync    *Resync
	fake      *forgejotest.Fake
	mirror    *fakeMirror
	files     *memFiles
	events    []string
	logs      []string
	calls     []restoreCall
	auths     []string
	made      int
	removed   []string
	readPaths []string
}

// newRunFixture wires a run for setup: GitHub and Forgejo hold the listings above, and the
// repository holds main's rule unless setup.noRule.
func newRunFixture(t *testing.T, setup runSetup) *runFixture {
	t.Helper()
	fx := &runFixture{fake: forgejotest.New()}
	fx.fake.AddRepo(liftRepo)
	if !setup.noRule {
		fx.fake.SeedRule(liftRepo, liftRule("main"))
	}
	fx.mirror = newFakeMirror(&fx.events)
	fx.mirror.remotes[runGitHub] = slices.Clone(runGitHubRefs)
	fx.mirror.remotes[runForgejo] = slices.Clone(runForgejoRefs)
	fx.files = &memFiles{files: map[string][]byte{}, events: &fx.events}
	getenv := func(name string) string {
		if name == setup.tokenVar {
			return forgejotest.AdminToken
		}
		return setup.env[name]
	}
	fx.resync = &Resync{
		Opts: &Options{
			Repo:       liftRepo,
			APIURL:     forgejotest.APIRoot + forgejotest.APIPath,
			MainBranch: "main",
			GitHubURL:  runGitHub,
			ForgejoURL: runForgejo,
			BackupDir:  liftBackupDir,
			DryRun:     setup.dryRun,
		},
		HTTP: &http.Client{Transport: authRecorder{
			auths: &fx.auths,
			next:  &liftTransport{fake: fx.fake, events: &fx.events},
		}},
		Files:  fx.files,
		Now:    func() time.Time { return liftNow },
		Log:    func(line string) { fx.logs = append(fx.logs, line) },
		Getenv: getenv,
		ReadFile: func(path string) ([]byte, error) {
			fx.readPaths = append(fx.readPaths, path)
			return nil, fs.ErrNotExist
		},
		Git: fx.mirror,
		MkdirTemp: func(string, string) (string, error) {
			fx.made++
			return runWork, nil
		},
		RemoveAll: func(path string) error {
			fx.removed = append(fx.removed, path)
			return nil
		},
		Provision: func(_ context.Context, opts *forgejoprov.Options, env forgejoprov.Env) int {
			fx.events = append(fx.events, "restore")
			fx.calls = append(fx.calls, restoreCall{opts: *opts, token: env.Getenv(restoreTokenEnv)})
			return forgejoprov.ExitOK
		},
	}
	return fx
}

// count returns how many recorded events start with prefix.
func (fx *runFixture) count(prefix string) int {
	n := 0
	for _, e := range fx.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// lastEvent returns the final recorded event, or "" when none was recorded.
func (fx *runFixture) lastEvent() string {
	if len(fx.events) == 0 {
		return ""
	}
	return fx.events[len(fx.events)-1]
}

// assertNoTokenAnywhere fails when the admin token shows in the progress log, the recorded
// events or the files written. The failure names the output, never its text.
func assertNoTokenAnywhere(t *testing.T, fx *runFixture) {
	t.Helper()
	texts := slices.Concat(fx.logs, fx.events)
	for path, data := range fx.files.files {
		texts = append(texts, path, string(data))
	}
	for _, text := range texts {
		if strings.Contains(text, forgejotest.AdminToken) {
			t.Fatal("admin token leaked into the run's output")
		}
	}
}

// TestRunConvergesAndIsIdempotent checks a run leaves Forgejo holding GitHub's refs, the rule
// lifted once and the provisioner restoring it, and that a second run over converged refs
// changes nothing. Mutation: drop the prune and the stale branch survives, so this goes red.
func TestRunConvergesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("first run = %d, want 0; logs %q", rc, fx.logs)
	}
	if !slices.Equal(fx.mirror.remotes[runForgejo], runGitHubRefs) {
		t.Fatalf("Forgejo refs = %v, want GitHub's %v", fx.mirror.remotes[runForgejo], runGitHubRefs)
	}
	for _, want := range []string{
		"will push refs/heads/main",
		"will delete refs/heads/stale-branch (absent from GitHub)",
		"+ git -C " + runScratch + " push --prune",
		"refs match: Forgejo holds every GitHub ref (3 refs)",
		"resync complete",
	} {
		if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, want) }) {
			t.Errorf("logs %q do not hold %q", fx.logs, want)
		}
	}
	if n := fx.count("DELETE "); n != 1 {
		t.Fatalf("rule deleted %d times, want 1", n)
	}
	if n := fx.count("push "); n != 1 {
		t.Fatalf("pushes = %d, want 1; events %v", n, fx.events)
	}
	push := slices.Index(fx.events, "push "+runForgejo+" "+strings.Join(Refspecs, " "))
	del := slices.IndexFunc(fx.events, func(e string) bool { return strings.HasPrefix(e, "DELETE ") })
	if push < 0 {
		t.Fatalf("push carries no refspecs: %v", fx.events)
	}
	if del < 0 || del > push {
		t.Fatalf("rule deleted at %d after the push at %d; events %v", del, push, fx.events)
	}
	if backup, ok := fx.files.files[liftFirstBackup]; !ok || !strings.Contains(string(backup), `"rule_name":"main"`) {
		t.Fatalf("no backup of the lifted rule at %s; writes %v", liftFirstBackup, fx.files.files)
	}
	if got := fx.calls; len(got) != 1 || !slices.Equal(got[0].opts.Repos, []string{liftRepo}) || got[0].opts.MainBranch != "main" {
		t.Fatalf("restore calls = %+v, want one for %s main", got, liftRepo)
	}
	assertNoTokenAnywhere(t, fx)

	fx.events, fx.logs, fx.mirror.changed = nil, nil, 0
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("second run = %d, want 0; logs %q", rc, fx.logs)
	}
	if fx.mirror.changed != 0 {
		t.Fatalf("second run changed %d refs, want 0", fx.mirror.changed)
	}
	for _, line := range fx.logs {
		if strings.HasPrefix(line, "will ") {
			t.Fatalf("second run planned %q over converged refs", line)
		}
	}
	if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, "refs match") }) {
		t.Fatalf("second run logs %q do not report matching refs", fx.logs)
	}
	if len(fx.calls) != 2 {
		t.Fatalf("provisioner ran %d times after two runs, want 2: main is restored on every run", len(fx.calls))
	}
}

// TestRunDryRunWritesNothing checks a dry run lists both sides and logs what it would do, and
// sends no write, creates no scratch directory and restores nothing. Mutation: send the push
// in a dry run and the push count goes red.
func TestRunDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv, dryRun: true})
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("dry run = %d, want 0; logs %q", rc, fx.logs)
	}
	if len(fx.events) != 2 || fx.count("ls-remote ") != 2 {
		t.Fatalf("dry run recorded %v, want only the two listings", fx.events)
	}
	if fx.made != 0 || len(fx.removed) != 0 {
		t.Fatalf("dry run made %d scratch dirs and removed %v, want none", fx.made, fx.removed)
	}
	if fx.fake.Rule(liftRepo, "main") == nil {
		t.Fatal("dry run lifted main's rule")
	}
	if len(fx.files.files) != 0 || len(fx.calls) != 0 {
		t.Fatalf("dry run wrote %v and ran the provisioner %d times", fx.files.files, len(fx.calls))
	}
	if !slices.Equal(fx.mirror.remotes[runForgejo], runForgejoRefs) {
		t.Fatalf("dry run changed Forgejo's refs to %v", fx.mirror.remotes[runForgejo])
	}
	n := len(fx.logs)
	if n < 2 || fx.logs[n-2] != "dry run: no write was sent" {
		t.Fatalf("logs end %q, want the no-write line before the restore line", fx.logs)
	}
	for _, want := range []string{
		"will push refs/heads/main",
		"will delete refs/heads/stale-branch (absent from GitHub)",
		"+ git -C <scratch> push --prune --quiet -- " + runForgejo,
		"+ DELETE /repos/acme/rig/branch_protections/main (dry run: not sent)",
		"+ restore main protection on acme/rig (dry run: not run)",
	} {
		if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, want) }) {
			t.Errorf("dry-run logs %q do not hold %q", fx.logs, want)
		}
	}
	assertNoTokenAnywhere(t, fx)
}

// TestRunVerifyFailureExitsOneAndStillRestores checks a push that leaves Forgejo behind GitHub
// fails the run, names the refs it disagrees on, and still restores main's protection.
// Mutation: skip the restore on the verify path and the restore count goes red.
func TestRunVerifyFailureExitsOneAndStillRestores(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	fx.mirror.noPush = true
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	if fx.lastEvent() != "restore" || len(fx.calls) != 1 {
		t.Fatalf("restore did not run last: events %v, calls %d", fx.events, len(fx.calls))
	}
	for _, want := range []string{
		"ERROR: Forgejo and GitHub refs differ after the push; a mirror sync would prune GitHub",
		"  refs/heads/main: GitHub aaaa1111, Forgejo dddd4444",
		"  refs/heads/stale-branch: GitHub absent, Forgejo eeee5555",
		"verify failed: refs differ after the push",
	} {
		if !slices.Contains(fx.logs, want) {
			t.Errorf("logs %q do not hold %q", fx.logs, want)
		}
	}
	if !slices.Equal(fx.removed, []string{runWork}) {
		t.Fatalf("scratch removed = %v, want %v", fx.removed, runWork)
	}
	assertNoTokenAnywhere(t, fx)
}

// TestRunStepFailureRestoresAndCleansUp checks a failed stage stops the run where it is: the
// restore still runs, the scratch directory goes when one was made, and the lift happens only
// when the run got to it. Mutation: lift before the fetch and the unreadable-GitHub row sends
// a DELETE, so it goes red.
func TestRunStepFailureRestoresAndCleansUp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		failAt        string
		failAfterPush string
		noScratch     bool
		failLine      string
		wantLift      int
		wantScratch   int
	}{
		{name: "GitHub unreadable", failAt: "ls-remote " + runGitHub, failLine: "list failed: ", wantLift: 0, wantScratch: 0},
		{name: "Forgejo unreadable", failAt: "ls-remote " + runForgejo, failLine: "list failed: ", wantLift: 0, wantScratch: 0},
		{name: "scratch dir cannot be made", noScratch: true, failLine: "fetch failed: make the scratch dir", wantLift: 0, wantScratch: 0},
		{name: "scratch repository cannot be made", failAt: "init", failLine: "fetch failed: ", wantLift: 0, wantScratch: 1},
		{name: "fetch fails", failAt: "fetch", failLine: "fetch failed: ", wantLift: 0, wantScratch: 1},
		{name: "push fails", failAt: "push", failLine: "push failed: ", wantLift: 1, wantScratch: 1},
		{name: "GitHub unreadable after the push", failAfterPush: "ls-remote " + runGitHub, failLine: "verify failed: ", wantLift: 1, wantScratch: 1},
		{name: "Forgejo unreadable after the push", failAfterPush: "ls-remote " + runForgejo, failLine: "verify failed: ", wantLift: 1, wantScratch: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
			fx.mirror.failAt = c.failAt
			fx.mirror.failAfterPush = c.failAfterPush
			if c.noScratch {
				fx.resync.MkdirTemp = func(string, string) (string, error) { return "", errors.New("disk full") }
			}
			if rc := fx.resync.Run(); rc != ExitFailure {
				t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
			}
			if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.HasPrefix(line, c.failLine) }) {
				t.Fatalf("logs %q do not name the failed stage %q", fx.logs, c.failLine)
			}
			if n := fx.count("DELETE "); n != c.wantLift {
				t.Fatalf("rule deleted %d times, want %d; events %v", n, c.wantLift, fx.events)
			}
			// The fake provisioner does not recreate the rule, so a rule still in place
			// means the lift never ran.
			if lifted := fx.fake.Rule(liftRepo, "main") == nil; lifted != (c.wantLift == 1) {
				t.Fatalf("main's rule lifted = %v, want %v", lifted, c.wantLift == 1)
			}
			if fx.lastEvent() != "restore" || len(fx.calls) != 1 {
				t.Fatalf("restore did not run last: events %v", fx.events)
			}
			if fx.made != c.wantScratch || len(fx.removed) != c.wantScratch {
				t.Fatalf("scratch made %d, removed %v; want %d of each", fx.made, fx.removed, c.wantScratch)
			}
			assertNoTokenAnywhere(t, fx)
		})
	}
}

// TestRunScratchRemovalFailureIsLogged checks a scratch directory the run cannot remove is named
// in the log without changing the result: the mirror landed, the restore still ran, and the run
// exits 0. Mutation: drop the log line in removeWork and the log check goes red.
func TestRunScratchRemovalFailureIsLogged(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	fx.resync.RemoveAll = func(path string) error {
		fx.removed = append(fx.removed, path)
		return errors.New("permission denied")
	}
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("Run = %d, want 0; logs %q", rc, fx.logs)
	}
	want := "could not remove the scratch dir " + runWork + ": permission denied"
	if !slices.Contains(fx.logs, want) {
		t.Fatalf("logs %q do not hold %q", fx.logs, want)
	}
	if fx.made != 1 || !slices.Equal(fx.removed, []string{runWork}) {
		t.Fatalf("scratch made %d, removal attempts %v; want 1 made and %s attempted", fx.made, fx.removed, runWork)
	}
	if fx.lastEvent() != "restore" || len(fx.calls) != 1 {
		t.Fatalf("restore did not run last: events %v", fx.events)
	}
}

// TestRunRefusesAListingThatWouldEmptyTheForgejoCopy checks a GitHub listing with no refs, or
// without main, stops the run before the fetch, the lift and the push, and that the restore
// still runs. Mutation: drop the guard and the empty row lifts main and prunes the copy to
// nothing, so the DELETE row goes red.
func TestRunRefusesAListingThatWouldEmptyTheForgejoCopy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		github  []Ref
		message string
	}{
		{name: "empty listing", github: nil, message: "GitHub's ref listing is empty"},
		{name: "listing without main", github: []Ref{{Name: "refs/heads/feature-a", SHA: "bbbb2222"}}, message: "GitHub's ref listing lacks refs/heads/main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
			fx.mirror.remotes[runGitHub] = c.github
			if rc := fx.resync.Run(); rc != ExitFailure {
				t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
			}
			if fx.count("DELETE ") != 0 || fx.count("fetch ") != 0 || fx.count("init ") != 0 || fx.count("push ") != 0 {
				t.Fatalf("refused run sent work: events %v", fx.events)
			}
			if fx.fake.Rule(liftRepo, "main") == nil {
				t.Fatal("refused run lifted main's rule")
			}
			if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, c.message) }) {
				t.Fatalf("logs %q do not name %q", fx.logs, c.message)
			}
			if fx.lastEvent() != "restore" {
				t.Fatalf("restore did not run last: events %v", fx.events)
			}
		})
	}
}

// TestRunGuardRefusesAPlanThatDeletesMain checks the plan guard refuses a delete list that names
// main and passes one that names only other refs. Behind the listing guard no plan built from a
// listing with main can name main, so this is the direct check on the guard's own branch.
func TestRunGuardRefusesAPlanThatDeletesMain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		del     []string
		wantErr bool
	}{
		{name: "plan deletes main", del: []string{"refs/heads/main"}, wantErr: true},
		{name: "plan deletes another ref", del: []string{"refs/heads/stale-branch"}, wantErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := guardPlan(runGitHubRefs, c.del, "main")
			if (err != nil) != c.wantErr {
				t.Fatalf("guardPlan = %v, want error %v", err, c.wantErr)
			}
		})
	}
}

// TestRunCanceledMidStageRestoresAndFails cancels the run's context while a stage is
// in flight: the call the stage waits on ends canceled, the stages after it never
// run, the restore runs once and Run exits 1. Mutation: drop the restore defer and
// the provisioner count goes red, which is the unprotected main this bead is about.
func TestRunCanceledMidStageRestoresAndFails(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.resync.Context = ctx
	ranAfter := false
	fx.resync.Steps = []Step{
		{Name: "list", Run: func() error {
			cancel()         // the signal lands while the stage's call is in flight
			return ctx.Err() // and the call ends with the run's context
		}},
		{Name: "push", Run: func() error { ranAfter = true; return nil }},
	}
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	if ranAfter {
		t.Fatal("a stage after the canceled one ran")
	}
	if len(fx.calls) != 1 || fx.lastEvent() != "restore" {
		t.Fatalf("restore ran %d times, last event %q; want one restore last", len(fx.calls), fx.lastEvent())
	}
}

// TestRunRestoresUnderItsOwnContextAfterCancellation pins that the cancellation does
// not reach the restore: the provisioner runs under a live context with its own
// bound, so a signal after the lift still puts main's protection back. Mutation:
// derive the restore's context from the run's and the recorded error is
// context.Canceled, the unprotected main this bead is about.
func TestRunRestoresUnderItsOwnContextAfterCancellation(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.resync.Context = ctx
	type restoreSeen struct {
		ctxErr  error
		bounded bool
	}
	seen := make(chan restoreSeen, 1)
	fx.resync.Provision = func(provCtx context.Context, opts *forgejoprov.Options, env forgejoprov.Env) int {
		_, bounded := provCtx.Deadline()
		seen <- restoreSeen{ctxErr: provCtx.Err(), bounded: bounded}
		fx.calls = append(fx.calls, restoreCall{opts: *opts, token: env.Getenv(restoreTokenEnv)})
		return forgejoprov.ExitOK
	}
	fx.resync.Steps = []Step{
		{Name: "list", Run: func() error {
			cancel()
			return ctx.Err()
		}},
	}
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	got := awaitReceive(t, seen)
	if got.ctxErr != nil {
		t.Fatalf("the restore's context error = %v, want nil: a canceled run must not cancel the restore", got.ctxErr)
	}
	if !got.bounded {
		t.Fatal("the restore's context has no bound of its own, want the restore timeout")
	}
	if len(fx.calls) != 1 {
		t.Fatalf("restore ran %d times, want 1", len(fx.calls))
	}
}

// TestRunCanceledBetweenStagesRunsNoFurtherStage: a stage that cancels the run and
// returns no error stops the stages after it, so a signal before the lift cannot
// take main's protection away. Mutation: drop the check before each stage and the
// push stage runs.
func TestRunCanceledBetweenStagesRunsNoFurtherStage(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.resync.Context = ctx
	ranAfter := false
	fx.resync.Steps = []Step{
		{Name: "list", Run: func() error { cancel(); return nil }},
		{Name: "push", Run: func() error { ranAfter = true; return nil }},
	}
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	if ranAfter {
		t.Fatal("the stage after the canceled one ran")
	}
	if !slices.Contains(fx.logs, "push not run: context canceled") {
		t.Fatalf("logs %q do not name the stage the cancellation stopped", fx.logs)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("restore ran %d times, want 1", len(fx.calls))
	}
}

// TestRunCanceledOnTheLastStageStillFails: a cancellation the last stage leaves
// behind makes the run report the cancellation rather than a convergence it did not
// see, and the restore still runs. Mutation: drop the check after the stages and
// Run returns 0.
func TestRunCanceledOnTheLastStageStillFails(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.resync.Context = ctx
	fx.resync.Steps = []Step{
		{Name: "verify", Run: func() error { cancel(); return nil }},
	}
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	if !slices.Contains(fx.logs, "run canceled: context canceled") {
		t.Fatalf("logs %q do not name the cancellation", fx.logs)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("restore ran %d times, want 1", len(fx.calls))
	}
}

// TestRunCanceledBeforeTheTokenSendsNothing: a run stopped before it starts reads no
// token, sends no request and runs no stage, and its exit is still the failure a
// signal asks for. Mutation: drop the opening check and the token is resolved and
// the stages run.
func TestRunCanceledBeforeTheTokenSendsNothing(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx.resync.Context = ctx
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	if len(fx.events) != 0 || len(fx.auths) != 0 || len(fx.calls) != 0 {
		t.Fatalf("a canceled run sent work: events %v, auths %v, calls %d", fx.events, fx.auths, len(fx.calls))
	}
	if len(fx.readPaths) != 0 {
		t.Fatalf("a canceled run read %v", fx.readPaths)
	}
	if !slices.Contains(fx.logs, "run canceled: context canceled") {
		t.Fatalf("logs %q do not name the cancellation", fx.logs)
	}
}

// TestRunWithoutAnAdminTokenSendsNothing checks a run with no token refuses before any request,
// any git call or the provisioner, names this command's broker rule, and reads no token file:
// a token left in $HOME is not a source. Mutation: fall back to the home token file and the
// read list goes red.
func TestRunWithoutAnAdminTokenSendsNothing(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: "", env: map[string]string{"HOME": "/home/decoy"}})
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run = %d, want 1; logs %q", rc, fx.logs)
	}
	if len(fx.events) != 0 || len(fx.auths) != 0 || len(fx.calls) != 0 {
		t.Fatalf("refused run sent work: events %v, auths %v, calls %d", fx.events, fx.auths, len(fx.calls))
	}
	if len(fx.readPaths) != 0 {
		t.Fatalf("refused run read %v; no token file was named", fx.readPaths)
	}
	if !slices.ContainsFunc(fx.logs, func(line string) bool {
		return strings.Contains(line, "no admin token") && strings.Contains(line, "sudo -u gt-forge gt-approve run forgejo-resync")
	}) {
		t.Fatalf("logs %q do not name the broker command", fx.logs)
	}
}

// TestRunPresentsTheResolvedTokenOnEveryCall checks every request carries the token the run
// resolved, whether the auth broker supplied it or a stand-in override did, and that the
// provisioner restore is handed the same token in memory. Mutation: send a request without the
// token and its Authorization header goes red.
func TestRunPresentsTheResolvedTokenOnEveryCall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tokenVar string
	}{
		{name: "broker token", tokenVar: restoreTokenEnv},
		{name: "stand-in override", tokenVar: "FORGEJO_ADMIN_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newRunFixture(t, runSetup{tokenVar: c.tokenVar})
			if rc := fx.resync.Run(); rc != ExitOK {
				t.Fatalf("Run = %d, want 0; logs %q", rc, fx.logs)
			}
			want := "token " + forgejotest.AdminToken
			if len(fx.auths) == 0 {
				t.Fatal("no request reached the API")
			}
			for _, auth := range fx.auths {
				if auth != want {
					t.Fatalf("request presented %q, want %q", auth, want)
				}
			}
			if len(fx.calls) != 1 || fx.calls[0].token != forgejotest.AdminToken {
				t.Fatalf("restore got token %v, want the resolved one", fx.calls)
			}
			if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, "refs match") }) {
				t.Fatalf("logs %q do not report matching refs", fx.logs)
			}
			assertNoTokenAnywhere(t, fx)
		})
	}
}

// TestRunRestoreTakesItsConfigDirFromTheEnvironment checks the restore runs the provisioner with
// the config directory the run's environment names, so its token files land there and not under
// HOME. Mutation: derive the config dir from HOME alone and the path goes red.
func TestRunRestoreTakesItsConfigDirFromTheEnvironment(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{
		tokenVar: restoreTokenEnv,
		env:      map[string]string{"XDG_CONFIG_HOME": "/cfg", "HOME": "/home/operator"},
	})
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("Run = %d, want 0; logs %q", rc, fx.logs)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("provisioner called %d times, want 1", len(fx.calls))
	}
	if got := fx.calls[0].opts.ConfigDir; got != "/cfg/gt" {
		t.Fatalf("ConfigDir = %q, want /cfg/gt", got)
	}
}

// TestRunWithNoRuleLiftsNothingAndRestores checks a repository with no main rule is a no-op lift,
// not an error: no DELETE is sent, the run converges and the restore still runs. Mutation: treat
// a missing rule as a failure and the exit code goes red.
func TestRunWithNoRuleLiftsNothingAndRestores(t *testing.T) {
	t.Parallel()
	fx := newRunFixture(t, runSetup{tokenVar: restoreTokenEnv, noRule: true})
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("Run = %d, want 0; logs %q", rc, fx.logs)
	}
	if n := fx.count("DELETE "); n != 0 {
		t.Fatalf("a rule-less repository got %d DELETEs", n)
	}
	if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, "nothing to lift") }) {
		t.Fatalf("logs %q do not report nothing to lift", fx.logs)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("restore ran %d times, want 1", len(fx.calls))
	}
}
