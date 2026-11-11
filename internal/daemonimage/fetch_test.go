package daemonimage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// testBDURL is the bd pin fetchBuilder fetches: a repository URL with no
// credential, the only form checkPinURL accepts.
const testBDURL = "http://forgejo:3000/owner/beads.git"

// fetchedGit is a git that answers HEAD with head in any directory gitHolding
// does not know as a checkout, so the tree a fetch leaves reads as head. It
// answers the pin-drift guard's main-tip fetch for that tree with a tip at head,
// the fresh case, and answers every other call as gitHolding over known.
func fetchedGit(run *fakeRunner, known map[string]string, head string) bdbuild.Git {
	g := run.git(known)
	return func(dir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == "HEAD^{commit}" {
			return []byte(head + "\n"), nil
		}
		if len(args) == 5 && args[0] == "fetch" && args[1] == "--depth" {
			return nil, nil
		}
		if len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == "origin/main^{commit}" {
			return []byte(head + "\n"), nil
		}
		return g(dir, args...)
	}
}

// fetchBuilder is a Builder whose bd has no checkout, so its pin is the only way
// the build gets bd; every other source is named. head is what git answers for
// the fetched tree's HEAD.
func fetchBuilder(t *testing.T, head string) (Builder, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{}
	return Builder{
		Run:      run.run,
		Git:      fetchedGit(run, map[string]string{testRev: testRev, testBDCommit: testBDCommit, testOMCommit: testOMCommit}, head),
		Environ:  testEnv,
		Out:      t.TempDir(),
		GTSrc:    "/src/gt",
		OMSrc:    "/src/om",
		OMCommit: testOMCommit,
		BDCommit: testBDCommit,
		Pins:     []Pin{{Name: "bd", URL: testBDURL, Commit: testBDCommit}},
	}, run
}

// TestFetchSourceByURLAtThePinnedCommit: with no --bd-src the build fetches the
// bd pin into an empty directory under Out, runs git init, a depth-1 fetch of
// the pinned commit from the pins URL and a detached checkout of FETCH_HEAD in
// that order, builds bd from the fetched tree, and removes it afterwards. A tree
// that is not at the pinned commit after the fetch is refused before any build.
func TestFetchSourceByURLAtThePinnedCommit(t *testing.T) {
	t.Parallel()

	t.Run("fetches the pin and builds from it", func(t *testing.T) {
		t.Parallel()
		b, run := fetchBuilder(t, testBDCommit)

		paths, err := b.BuildBinaries(context.Background(), testRev)
		if err != nil {
			t.Fatalf("BuildBinaries: %v", err)
		}

		want := []struct{ argv []string }{
			{[]string{"git", "init"}},
			{[]string{"git", "fetch", "--depth", "1", testBDURL, testBDCommit}},
			{[]string{"git", "checkout", "--detach", "FETCH_HEAD"}},
		}
		if len(run.calls) < len(want) {
			t.Fatalf("the build ran %d commands, want the three fetches first: %v", len(run.calls), run.calls)
		}
		dir := run.calls[0].dir
		if !strings.HasPrefix(dir, b.Out+string(filepath.Separator)) || !strings.Contains(filepath.Base(dir), ".daemonimage-src-bd-") {
			t.Errorf("the fetch ran in %q, want a fresh directory under the build context %q", dir, b.Out)
		}
		for i, w := range want {
			got := run.calls[i]
			if strings.Join(got.argv, " ") != strings.Join(w.argv, " ") {
				t.Errorf("fetch command %d = %v, want %v", i, got.argv, w.argv)
			}
			if got.dir != dir {
				t.Errorf("fetch command %d ran in %q, want the one directory %q", i, got.dir, dir)
			}
			for _, a := range got.argv {
				if strings.Contains(a, "@") {
					t.Errorf("fetch argv holds %q, which a credential would look like", a)
				}
			}
		}
		if len(run.adds) < 2 || run.adds[1].src != dir {
			t.Errorf("the bd worktrees were made from %v, want the fetched tree %s", run.adds, dir)
		}
		if len(paths) != 3 {
			t.Errorf("BuildBinaries returned %v, want the three binaries", paths)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the fetched tree %s is still in the build context after the build (err %v)", dir, err)
		}
	})

	t.Run("refuses a tree that is not at the pin", func(t *testing.T) {
		t.Parallel()
		b, run := fetchBuilder(t, testOtherCommit)

		if _, err := b.BuildBinaries(context.Background(), testRev); err == nil || !strings.Contains(err.Error(), "not the pinned") {
			t.Fatalf("BuildBinaries = %v, want a refusal naming the tree's commit", err)
		}
		if len(run.calls) == 0 {
			t.Fatal("the fetch ran no command")
		}
		for _, call := range run.calls {
			if len(call.argv) > 0 && call.argv[0] == "go" {
				t.Errorf("a build ran on a tree that is not the pin: %v", call.argv)
			}
		}
		if _, err := os.Stat(run.calls[0].dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused fetch left %s in the build context (err %v)", run.calls[0].dir, err)
		}
	})
}

// TestFetchSourceKeepsACheckoutFlagExactly: with --bd-src named the build issues
// no fetch command at all and builds from that checkout, and the canonical-clone
// refusal still applies to it.
func TestFetchSourceKeepsACheckoutFlagExactly(t *testing.T) {
	t.Parallel()
	run := &fakeRunner{}
	b := validBuilder(run, t.TempDir())
	b.Pins = planPins

	paths, err := b.BuildBinaries(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	for _, call := range run.calls {
		if call.argv[0] != "go" {
			t.Errorf("the build ran %v with a checkout named; want only go builds", call.argv)
		}
	}
	for _, add := range run.adds {
		if add.src != b.BDSrc && add.src != b.OMSrc && add.src != b.GTSrc {
			t.Errorf("a worktree was made from %q, want a named checkout", add.src)
		}
	}
	if len(paths) != 3 {
		t.Errorf("BuildBinaries returned %v, want the three binaries", paths)
	}

	clone := filepath.Join(t.TempDir(), "town", "gastown", "mayor", "rig")
	refused := &fakeRunner{}
	canonical := validBuilder(refused, t.TempDir())
	canonical.Pins = planPins
	canonical.BDSrc = clone
	if _, err := canonical.BuildBinaries(context.Background(), testRev); err == nil || !strings.Contains(err.Error(), "canonical clone") {
		t.Fatalf("BuildBinaries with a canonical clone as --bd-src = %v, want the canonical-clone refusal", err)
	}
	if len(refused.calls) != 0 {
		t.Errorf("a refused build ran %v", refused.calls)
	}
}

// staleGuard is what the pin-drift guard reads for a fetched source: whether the
// tip fetch fails, whether the tip cannot be read, and the shape of the pin
// against the tip.
type staleGuard struct {
	tip         string
	tipFetchErr error
	tipReadErr  bool
	notAncestor bool
	behind      int
	age         time.Duration
	now         time.Time
	fetches     [][]string
}

// fetchedCommit returns the commit run recorded fetching into dir.
func fetchedCommit(run *fakeRunner, dir string) (string, bool) {
	for _, call := range run.calls {
		if call.dir == dir && len(call.argv) == 6 && call.argv[0] == "git" && call.argv[1] == "fetch" && call.argv[3] == "1" {
			return call.argv[5], true
		}
	}
	return "", false
}

// guardGit is a git over the fetched trees a run recorded a fetch in: it answers
// the tree's HEAD, its clean status, and the pin-drift guard's tip fetch and
// rules from g, and answers every other call as inner.
func guardGit(run *fakeRunner, inner bdbuild.Git, g *staleGuard) bdbuild.Git {
	return func(dir string, args ...string) ([]byte, error) {
		pin, fetched := fetchedCommit(run, dir)
		if !fetched {
			return inner(dir, args...)
		}
		switch {
		case len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == "HEAD^{commit}":
			return []byte(pin + "\n"), nil
		case args[0] == "status":
			return nil, nil
		case len(args) == 5 && args[0] == "fetch" && args[1] == "--depth":
			g.fetches = append(g.fetches, append([]string(nil), args...))
			if g.tipFetchErr != nil {
				return nil, g.tipFetchErr
			}
			return nil, nil
		case len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == "origin/main^{commit}":
			if g.tipReadErr {
				return nil, errors.New("fatal: bad revision 'origin/main'")
			}
			return []byte(g.tip + "\n"), nil
		case args[0] == "merge-base":
			if g.notAncestor {
				return nil, exitStatus(1)
			}
			return nil, nil
		case args[0] == "rev-list":
			return []byte(strconv.Itoa(g.behind) + "\n"), nil
		case args[0] == "show":
			if args[len(args)-1] == pin {
				return []byte(strconv.FormatInt(g.now.Add(-g.age).Unix(), 10) + "\n"), nil
			}
			return []byte(strconv.FormatInt(g.now.Unix(), 10) + "\n"), nil
		}
		return inner(dir, args...)
	}
}

// TestFetchedPinsAreStalenessChecked: a source the build fetches by URL is
// checked against the main tip fetched from the same pin URL with the rules
// CheckPins applies to a checkout. Every fetched source is checked, a fresh pin
// passes, and a pin that is not an ancestor of the tip, one more than the
// 40-commit bound behind it, one more than the 7-day bound older than it, a tip
// fetch that fails and a tip that cannot be read are each refused naming the
// source, never reported as unchecked.
func TestFetchedPinsAreStalenessChecked(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_790_000_000, 0)
	// testOMCommit and testHMCommit are the same sha, so the map names it once.
	known := map[string]string{
		testRev: testRev, testBDCommit: testBDCommit, testOMCommit: testOMCommit,
		testDevopsCommit: testDevopsCommit,
	}

	t.Run("every source fetched is checked", func(t *testing.T) {
		t.Parallel()
		run := &fakeRunner{}
		guard := &staleGuard{tip: testOtherCommit, behind: 3, age: 2 * 24 * time.Hour, now: now}
		b := Builder{
			Run: run.run, Git: guardGit(run, run.git(known), guard),
			Go: versionGo(versionInfo(testReleaseVersion)), Environ: testEnv, Out: t.TempDir(),
			GTSrc:    "/src/gt",
			BDCommit: testBDCommit, OMCommit: testOMCommit,
			HMCommit: testHMCommit, DevopsCommit: testDevopsCommit,
			Pins: []Pin{
				{Name: "bd", URL: "http://forgejo:3000/owner/beads.git", Commit: testBDCommit},
				{Name: "om", URL: "http://forgejo:3000/owner/organic-mechanic.git", Commit: testOMCommit},
				{Name: "hm", URL: "http://forgejo:3000/owner/history-man.git", Commit: testHMCommit},
				{Name: "devops", URL: "http://forgejo:3000/owner/devops.git", Commit: testDevopsCommit},
			},
		}

		if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
			t.Fatalf("BuildBinaries: %v", err)
		}
		if _, err := b.BuildTools(context.Background()); err != nil {
			t.Fatalf("BuildTools: %v", err)
		}

		if len(guard.fetches) != len(PinNames) {
			t.Fatalf("the guard fetched %d main tips, want one per fetched source (%d): %v", len(guard.fetches), len(PinNames), guard.fetches)
		}
		wantDepth := strconv.Itoa(MaxPinBehind + 1)
		for _, argv := range guard.fetches {
			if argv[1] != "--depth" || argv[2] != wantDepth || argv[4] != "main:refs/remotes/origin/main" {
				t.Errorf("tip fetch = %v, want --depth %s main:refs/remotes/origin/main", argv, wantDepth)
			}
		}
	})

	refusal := func(t *testing.T, guard staleGuard) error {
		t.Helper()
		run := &fakeRunner{}
		guard.now = now
		guard.tip = testOtherCommit
		b := Builder{
			Run: run.run, Git: guardGit(run, run.git(known), &guard),
			Environ: testEnv, Out: t.TempDir(), GTSrc: "/src/gt",
			OMSrc: "/src/om", OMCommit: testOMCommit, BDCommit: testBDCommit,
			Pins: []Pin{{Name: "bd", URL: testBDURL, Commit: testBDCommit}},
		}
		if _, err := b.BuildBinaries(context.Background(), testRev); err != nil {
			for _, call := range run.calls {
				if len(call.argv) != 0 && call.argv[0] == "go" {
					t.Errorf("a build ran on a refused pin: %v", call.argv)
				}
			}
			if len(run.calls) != 0 {
				dir := run.calls[0].dir
				if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("a refused fetched pin left %s in the build context (err %v)", dir, statErr)
				}
			}
			return err
		}
		t.Fatal("BuildBinaries passed a fetched pin the guard refuses")
		return nil
	}

	for _, tc := range []struct {
		name  string
		guard staleGuard
		want  []string
	}{
		{
			name:  "a pin that is not an ancestor of the fetched tip",
			guard: staleGuard{notAncestor: true},
			want:  []string{"pin-drift guard:", "rig bd pin", "is not an ancestor of main"},
		},
		{
			name:  "a pin 41 commits behind the fetched tip",
			guard: staleGuard{behind: MaxPinBehind + 1},
			want:  []string{"pin-drift guard:", "rig bd pin", "is 41 commits behind", "over the 40-commit bound"},
		},
		{
			name:  "a pin more than 7 days older than the fetched tip",
			guard: staleGuard{behind: 3, age: MaxPinAge + 24*time.Hour},
			want:  []string{"pin-drift guard:", "rig bd pin", "older than main's tip", "over the " + MaxPinAge.String() + " bound"},
		},
		{
			name:  "a tip fetch that fails",
			guard: staleGuard{tipFetchErr: errors.New("fatal: could not fetch")},
			want:  []string{"pin-drift guard:", "cannot fetch main of the bd pin"},
		},
		{
			name:  "a tip that cannot be read",
			guard: staleGuard{tipReadErr: true},
			want:  []string{"pin-drift guard:", "cannot read the main tip fetched for the bd pin"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := refusal(t, tc.guard)
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal = %v, want it to contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "unchecked") {
				t.Errorf("refusal = %v, want a refusal, not an unchecked pin", err)
			}
		})
	}
}

// planArgv returns the argv a plan line prints: the text after the line's label
// and before the note Plan puts in parentheses.
func planArgv(line string) string {
	_, rest, ok := strings.Cut(line, ": ")
	if !ok {
		return ""
	}
	argv, _, _ := strings.Cut(rest, "   (")
	return argv
}

// engineArgv reports whether argv is a container-engine command: docker running
// a build, run, tag, push or load.
func engineArgv(argv string) bool {
	fields := strings.Fields(argv)
	if len(fields) == 0 || fields[0] != "docker" {
		return false
	}
	for _, f := range fields[1:] {
		switch f {
		case "build", "run", "tag", "push", "load":
			return true
		}
	}
	return false
}

// TestContextOnlyIssuesNoEngineCommand: the plan of a context-only build names no
// container-engine command across the whole build, whatever source flags are
// named, and its last line names the absolute context directory. It still plans
// every step that stages the context.
func TestContextOnlyIssuesNoEngineCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		noSrc  bool
		source string
	}{
		{name: "every source checked out"},
		{name: "the bd source fetched by URL", noSrc: true, source: "fetch bd: git init"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := planBuilder(t)
			if tc.noSrc {
				b.BDSrc = ""
			}
			lines, err := b.PlanContext(testRev)
			if err != nil {
				t.Fatalf("PlanContext: %v", err)
			}
			if len(lines) == 0 {
				t.Fatal("PlanContext listed nothing")
			}
			for _, line := range lines {
				if engineArgv(planArgv(line)) {
					t.Errorf("the context-only plan runs the container engine: %q", line)
				}
			}
			if last := lines[len(lines)-1]; last != ContextLine(b.Out) {
				t.Errorf("the last line is %q, want the context directory %q", last, ContextLine(b.Out))
			}
			if !filepath.IsAbs(b.Out) {
				t.Fatalf("the context %q is not absolute", b.Out)
			}
			want := []string{"build gt: go build", "build bd: go build", "build om: go build",
				"build app-promote: go build", "build hm: go build",
				"fetch claude: GET ", "fetch kubectl: GET "}
			if tc.source != "" {
				want = append(want, tc.source)
			}
			for _, w := range want {
				if !hasPlanLine(lines, w) {
					t.Errorf("the context-only plan lacks %q:\n%s", w, strings.Join(lines, "\n"))
				}
			}
		})
	}
}

// hasPlanLine reports whether one line of lines starts with prefix.
func hasPlanLine(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// TestNoCredentialInAnyArgv: a pins URL carrying a credential is refused before
// any command runs and before a plan line could print it, and no refusal echoes
// the URL. A credential-free pin's argv and plan line hold no "@" at all.
func TestNoCredentialInAnyArgv(t *testing.T) {
	t.Parallel()
	const credentialURL = "http://user:s3cret@forgejo:3000/owner/beads.git"

	b := planBuilder(t)
	b.BDSrc = ""
	b.Pins = append([]Pin(nil), planPins...)
	b.Pins[0].URL = credentialURL

	lines, err := b.Plan(testRev)
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("Plan = %q, %v; want a refusal naming the credential", lines, err)
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "@") {
		t.Errorf("the refusal echoes the URL: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("a refused plan printed %q", lines)
	}

	run := &fakeRunner{}
	b.Run = run.run
	b.Git = run.git(map[string]string{testRev: testRev, testBDCommit: testBDCommit, testOMCommit: testOMCommit})
	if _, err := b.BuildBinaries(context.Background(), testRev); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("BuildBinaries = %v, want the credential refusal", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused fetch ran %v", run.calls)
	}

	// A named checkout's pin URL is not the plan's to check: the pins file is
	// validated at parse, and only the pins a build fetches are refused here.
	named := planBuilder(t)
	named.Pins = append([]Pin(nil), planPins...)
	named.Pins[0].URL = credentialURL
	if _, err := named.Plan(testRev); err != nil {
		t.Fatalf("Plan with a named checkout = %v, want the named checkout used as it is", err)
	}

	dir := filepath.Join(t.TempDir(), "src")
	err = FetchSource(context.Background(), run.run, run.git(nil), dir, testEnv, Pin{Name: "bd", URL: credentialURL, Commit: testBDCommit})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("FetchSource = %v, want the credential refusal", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused fetch ran %v", run.calls)
	}

	good, goodRun := fetchBuilder(t, testBDCommit)
	if _, err := good.BuildBinaries(context.Background(), testRev); err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	for _, call := range goodRun.calls {
		for _, a := range call.argv {
			if strings.Contains(a, "@") {
				t.Errorf("a credential-free fetch ran argv %q", call.argv)
			}
		}
	}
}

// TestFetchSourceRefusesWhatItCannotFetch: each input a fetch cannot use is
// refused before or at the step that needs it, and a directory that already
// holds something is left as it was: what is in it is not the fetch's to remove.
func TestFetchSourceRefusesWhatItCannotFetch(t *testing.T) {
	t.Parallel()
	good := Pin{Name: "bd", URL: testBDURL, Commit: testBDCommit}
	for _, tc := range []struct {
		name string
		run  func(*fakeRunner) Runner
		git  func(*fakeRunner) bdbuild.Git
		pin  Pin
		// setup prepares dir before the fetch; absent leaves dir missing.
		setup func(t *testing.T, dir string)
		want  string
	}{
		{
			name: "no runner",
			run:  func(*fakeRunner) Runner { return nil },
			git:  func(r *fakeRunner) bdbuild.Git { return fetchedGit(r, nil, "") },
			pin:  good,
			want: "no runner",
		},
		{
			name: "no git",
			run:  func(r *fakeRunner) Runner { return r.run },
			git:  func(*fakeRunner) bdbuild.Git { return nil },
			pin:  good,
			want: "no git",
		},
		{
			name: "a URL that is not http",
			run:  func(r *fakeRunner) Runner { return r.run },
			git:  func(r *fakeRunner) bdbuild.Git { return fetchedGit(r, nil, "") },
			pin:  Pin{Name: "bd", URL: "ssh://forgejo/owner/beads.git", Commit: testBDCommit},
			want: "not http:// or https://",
		},
		{
			name: "a commit that is not a full sha",
			run:  func(r *fakeRunner) Runner { return r.run },
			git:  func(r *fakeRunner) bdbuild.Git { return fetchedGit(r, nil, "") },
			pin:  Pin{Name: "bd", URL: testBDURL, Commit: "fedcba9"},
			want: "not a full 40-character hex sha",
		},
		{
			name: "a directory that does not exist",
			run:  func(r *fakeRunner) Runner { return r.run },
			git:  func(r *fakeRunner) bdbuild.Git { return fetchedGit(r, nil, "") },
			pin:  good,
			want: "cannot read the fetch directory",
		},
		{
			name:  "a directory that already holds something",
			run:   func(r *fakeRunner) Runner { return r.run },
			git:   func(r *fakeRunner) bdbuild.Git { return fetchedGit(r, nil, "") },
			pin:   good,
			setup: func(t *testing.T, dir string) { t.Helper(); writeTestFile(t, filepath.Join(dir, "stale"), "x") },
			want:  "a fetch needs an empty directory",
		},
		{
			name: "a fetch command that fails",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			run: func(r *fakeRunner) Runner {
				return func(ctx context.Context, dir string, env, argv []string) error {
					if err := r.run(ctx, dir, env, argv); err != nil {
						return err
					}
					if argv[1] == "fetch" {
						return errors.New("fatal: could not fetch")
					}
					return nil
				}
			},
			git:  func(r *fakeRunner) bdbuild.Git { return fetchedGit(r, nil, "") },
			pin:  good,
			want: "git fetch --depth 1",
		},
		{
			name: "a HEAD that cannot be read",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			run: func(r *fakeRunner) Runner { return r.run },
			git: func(*fakeRunner) bdbuild.Git {
				return func(string, ...string) ([]byte, error) { return nil, errors.New("fatal: bad object") }
			},
			pin:  good,
			want: "cannot read the commit fetched",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{}
			dir := filepath.Join(t.TempDir(), "src")
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			if err := FetchSource(context.Background(), tc.run(run), tc.git(run), dir, testEnv, tc.pin); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("FetchSource = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestFetchSourceRefusesAnOutputThatCannotHoldIt: the directory a fetched source
// is checked out in is made under Out, so an Out that cannot hold one stops the
// build before the fetch runs.
func TestFetchSourceRefusesAnOutputThatCannotHoldIt(t *testing.T) {
	t.Parallel()
	b, run := fetchBuilder(t, testBDCommit)
	b.Out = filepath.Join(b.Out, "absent")

	if _, err := b.BuildBinaries(context.Background(), testRev); err == nil || !strings.Contains(err.Error(), "cannot make a fetch directory") {
		t.Fatalf("BuildBinaries = %v, want a refusal naming the fetch directory", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("a refused fetch ran %v", run.calls)
	}
}

// writeTestFile writes body to path, making the directory that holds it.
func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
