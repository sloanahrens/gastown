package imagectx

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat/seed"
)

// The fixture is a world of commits, mains and a clean gt checkout, held in
// memory. Every seam answers from it and records its call, so a test reads what
// the build ran and proves no engine was reached. Nothing here touches the disk
// except the output directory the test makes, the network or the host's git.

const (
	srcDir    = "/fx/gastown"
	beadsURL  = "http://fx.invalid/beads.git"
	devopsURL = "http://fx.invalid/devops.git"

	gastownRev = "0000000000000000000000000000000000000001"
	bdPin      = "0000000000000000000000000000000000000010"
	devopsPin  = "0000000000000000000000000000000000000020"
	beadsPin   = "0000000000000000000000000000000000000030"
	beadsMain  = "0000000000000000000000000000000000000031"
	devopsNew  = "0000000000000000000000000000000000000041"
)

// engines are the container tools the image build would call. No recorded call
// may name one.
var engines = []string{"docker", "podman", "nerdctl", "buildctl", "ctr", "crictl"}

// goMod is a go.mod with a go line and the requires given.
func goMod(module, goLine string, requires ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n", module, goLine)
	for _, r := range requires {
		fmt.Fprintf(&b, "require %s\n", r)
	}
	return b.String()
}

// tarOf is a `git archive --format=tar` stream holding files.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// world is the in-memory repositories and the calls made against them.
type world struct {
	t       *testing.T
	head    string
	dirty   bool
	commits map[string]map[string]string
	// mains maps a URL to the sha its refs/heads/main answers with.
	mains map[string]string
	// moveAfterLsRemote, when set for a URL, makes its main move after the
	// first ls-remote answer: the fetch of main then finds a different tip.
	moveAfterLsRemote map[string]string
	// fetched is the commit each fetch directory's HEAD and FETCH_HEAD name.
	fetched map[string]string
	calls   []string
	// goCalls are the go invocations, separate from calls so a test can assert
	// none ran.
	goCalls []string
	// goEnvs is the environment each go call ran with, parallel to goCalls.
	goEnvs [][]string
	// resolve maps a commit a rev-parse asks for to the commit git answers
	// with, so a test can make a pin resolve to another commit.
	resolve map[string]string
}

// newWorld is the default world: a clean gt checkout at gastownRev, bd and the
// two seeded rigs at their pins, and beads' main ahead of its pin.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		t:                 t,
		head:              gastownRev,
		commits:           map[string]map[string]string{},
		mains:             map[string]string{},
		moveAfterLsRemote: map[string]string{},
		fetched:           map[string]string{},
		resolve:           map[string]string{},
	}
	w.commits[gastownRev] = map[string]string{
		"go.mod":                     goMod("github.com/steveyegge/gastown", "1.9.0"),
		"deploy/polecat/go-floor.sh": "# the image's Go floor check, as the commit holds it\n",
		"cmd/gt/main.go":             "package main\n",
	}
	w.commits[bdPin] = map[string]string{"go.mod": goMod("github.com/steveyegge/beads", "1.10.0")}
	w.commits[devopsPin] = map[string]string{"go.mod": goMod("github.com/steveyegge/devops", "1.8.0", "example.invalid/dep v1.0.0")}
	w.commits[beadsPin] = map[string]string{"go.mod": goMod("github.com/steveyegge/beads", "1.10.0", "example.invalid/dep v1.0.0")}
	w.commits[beadsMain] = map[string]string{"go.mod": goMod("github.com/steveyegge/beads", "1.10.0", "example.invalid/dep v1.0.0")}
	w.mains[devopsURL] = devopsPin
	w.mains[beadsURL] = beadsMain
	return w
}

// inputs is the build the default world stages into out.
func (w *world) inputs(out string) Inputs {
	return Inputs{
		Src: srcDir,
		Rev: gastownRev,
		Out: out,
		BD:  daemonimage.Pin{Name: "bd", URL: beadsURL, Commit: bdPin},
		Seeds: []seed.Source{
			{Rig: BuildRig, Commit: "0000000000000000000000000000000000000099"},
			{Rig: "devops", Commit: devopsPin},
			{Rig: "beads", Commit: beadsPin},
		},
		SeedURLs: map[string]string{"devops": devopsURL, "beads": beadsURL},
	}
}

func (w *world) seams() Seams {
	return Seams{Run: w.runCmd, Git: w.gitCmd, Go: w.goCmd}
}

func (w *world) runCmd(_ context.Context, dir string, _ []string, argv []string) error {
	w.calls = append(w.calls, fmt.Sprintf("run %s %s", dir, strings.Join(argv, " ")))
	if len(argv) >= 5 && argv[0] == "git" && argv[1] == "fetch" {
		url, ref := argv[len(argv)-2], argv[len(argv)-1]
		if argv[2] == "--unshallow" {
			w.fetched[dir] = w.mains[url]
			return nil
		}
		if _, ok := w.commits[ref]; !ok {
			return fmt.Errorf("fetch: no commit %s at %s", ref, url)
		}
		w.fetched[dir] = ref
	}
	return nil
}

func (w *world) gitCmd(dir string, args ...string) ([]byte, error) {
	w.calls = append(w.calls, fmt.Sprintf("git %s %s", dir, strings.Join(args, " ")))
	switch args[0] {
	case "status":
		if dir == srcDir && w.dirty {
			return []byte(" M cmd/gt/main.go\n"), nil
		}
		return nil, nil
	case "ls-remote":
		url := args[1]
		main := w.mains[url]
		if next, ok := w.moveAfterLsRemote[url]; ok {
			w.mains[url] = next
			delete(w.moveAfterLsRemote, url)
		}
		return []byte(main + "\trefs/heads/main\n"), nil
	case "archive":
		commit := args[len(args)-1]
		files, ok := w.commits[commit]
		if !ok {
			return nil, fmt.Errorf("archive: no commit %s", commit)
		}
		return tarOf(w.t, files), nil
	case "show":
		if args[1] == "-s" {
			return []byte("1000\n"), nil
		}
		commit, name, _ := strings.Cut(args[1], ":")
		body, ok := w.commits[commit][name]
		if !ok {
			return nil, fmt.Errorf("show: %s not in %s", name, commit)
		}
		return []byte(body), nil
	case "ls-tree":
		commit := args[len(args)-1]
		var names []string
		for name := range w.commits[commit] {
			names = append(names, name)
		}
		return []byte(strings.Join(names, "\n")), nil
	case "merge-base":
		return nil, nil
	case "rev-list":
		return []byte("2\n"), nil
	case "worktree":
		return nil, nil
	case "rev-parse":
		last := args[len(args)-1]
		switch {
		case args[1] == "--short":
			return []byte(args[2][:7] + "\n"), nil
		case last == "FETCH_HEAD^{commit}":
			return []byte(w.fetched[dir] + "\n"), nil
		case last == "HEAD^{commit}" && dir == srcDir:
			return []byte(w.head + "\n"), nil
		case last == "HEAD^{commit}":
			return []byte(w.fetched[dir] + "\n"), nil
		}
		commit := strings.TrimSuffix(last, "^{commit}")
		if r, ok := w.resolve[commit]; ok {
			commit = r
		}
		if _, ok := w.commits[commit]; !ok {
			return nil, fmt.Errorf("rev-parse: no commit %s", commit)
		}
		return []byte(commit + "\n"), nil
	}
	return nil, fmt.Errorf("fake git: unexpected verb %q", args[0])
}

func (w *world) goCmd(dir string, env []string, args ...string) ([]byte, error) {
	w.goCalls = append(w.goCalls, fmt.Sprintf("go %s %s", dir, strings.Join(args, " ")))
	w.goEnvs = append(w.goEnvs, env)
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			if err := os.MkdirAll(filepath.Dir(args[i+1]), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(args[i+1], []byte("bin\n"), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

// envOfCall is the environment of the one go call whose argv names pkg, failing
// the test when no call or more than one names it.
func envOfCall(t *testing.T, w *world, pkg string) []string {
	t.Helper()
	var found []string
	n := 0
	for i, c := range w.goCalls {
		if strings.HasSuffix(c, " "+pkg) {
			found = w.goEnvs[i]
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d go calls build %s, want one\n%s", n, pkg, w.dump())
	}
	return found
}

// saw reports whether a run or git call contains substr.
func (w *world) saw(substr string) bool {
	for _, c := range w.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (w *world) dump() string {
	return strings.Join(w.calls, "\n") + "\n" + strings.Join(w.goCalls, "\n")
}

// TestContextFromPinsFetchesEverySourceByURL checks each source with no checkout
// is fetched by URL at its pin, the seeded rigs' mains are read by URL, and the
// gt binaries are built from the clean checkout at --rev. A dirty tree is
// refused before any build.
func TestContextFromPinsFetchesEverySourceByURL(t *testing.T) {
	t.Parallel()
	t.Run("fetches each source at its pin by URL", func(t *testing.T) {
		w := newWorld(t)
		out := t.TempDir()
		if _, err := Build(context.Background(), w.inputs(out), w.seams()); err != nil {
			t.Fatalf("Build: %v\n%s", err, w.dump())
		}
		for _, want := range []string{
			"fetch --depth 1 " + beadsURL + " " + bdPin,
			"fetch --depth 1 " + devopsURL + " " + devopsPin,
			"fetch --depth 1 " + beadsURL + " " + beadsPin,
			"ls-remote " + devopsURL + " refs/heads/main",
			"ls-remote " + beadsURL + " refs/heads/main",
			"fetch --unshallow " + beadsURL + " refs/heads/main",
		} {
			if !w.saw(want) {
				t.Errorf("no call %q\n%s", want, w.dump())
			}
		}
	})

	t.Run("reads the build rig from the checkout at --rev and never fetches it", func(t *testing.T) {
		w := newWorld(t)
		out := t.TempDir()
		if _, err := Build(context.Background(), w.inputs(out), w.seams()); err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !w.saw("git " + srcDir + " archive --format=tar " + gastownRev) {
			t.Errorf("the build rig's tree at --rev was not archived from the checkout\n%s", w.dump())
		}
		if !w.saw("git " + srcDir + " show " + gastownRev + ":deploy/polecat/go-floor.sh") {
			t.Errorf("go-floor.sh was not read from the checkout at --rev\n%s", w.dump())
		}
		for _, c := range w.calls {
			if strings.HasPrefix(c, "run ") && strings.Contains(c, " fetch ") &&
				!strings.Contains(c, beadsURL) && !strings.Contains(c, devopsURL) {
				t.Errorf("a fetch named a URL other than the pinned sources: %s", c)
			}
		}
		want := "go " + srcDir + " build -trimpath -o " + filepath.Join(out, "bin", "gt") + " ./cmd/gt"
		if !contains(w.goCalls, want) {
			t.Errorf("gt was not built from the checkout at --rev\nwant %q\n%s", want, w.dump())
		}
	})

	t.Run("refuses a dirty tree before any build", func(t *testing.T) {
		w := newWorld(t)
		w.dirty = true
		_, err := Build(context.Background(), w.inputs(t.TempDir()), w.seams())
		if err == nil || !strings.Contains(err.Error(), "uncommitted or untracked changes") {
			t.Fatalf("Build error = %v, want the dirty-tree refusal", err)
		}
		if len(w.goCalls) != 0 {
			t.Errorf("a dirty tree still ran a build: %v", w.goCalls)
		}
	})
}

// TestContextHoldsThePayloadTheDockerfileCopies checks the context holds every
// path the polecat Dockerfile copies, bd.commit and the seed directory, and that
// its printed labels carry the gt commit, the bd commit and the Go floor, the
// highest go line numerically (1.10.0, not 1.9.0).
func TestContextHoldsThePayloadTheDockerfileCopies(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	out := t.TempDir()
	res, err := Build(context.Background(), w.inputs(out), w.seams())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, rel := range []string{
		"go-floor.sh",
		"bd.commit",
		filepath.Join("bin", "gt"),
		filepath.Join("bin", "bd"),
		filepath.Join("bin", "polecat-image"),
		filepath.Join("bin", "sweep-run"),
		filepath.Join("seed", "gastown", "go.mod"),
		filepath.Join("seed", "devops", "go.mod"),
		filepath.Join("seed", "beads", "go.mod"),
	} {
		if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
			t.Errorf("the context has no %s: %v", rel, err)
		}
	}
	bdCommit, err := os.ReadFile(filepath.Join(out, "bd.commit"))
	if err != nil || strings.TrimSpace(string(bdCommit)) != bdPin {
		t.Errorf("bd.commit = %q (%v), want %s", bdCommit, err, bdPin)
	}
	for _, want := range []string{
		"context: " + out,
		"label: org.gastown.gt-commit=" + gastownRev,
		"label: org.gastown.bd-commit=" + bdPin,
		"label: go-floor=1.10.0",
	} {
		if !contains(res.Lines(), want) {
			t.Errorf("the printed lines lack %q\n%s", want, strings.Join(res.Lines(), "\n"))
		}
	}
	// The gt, polecat-image and sweep-run binaries are cross-compiled for the
	// image's platform by the build's environment, not by the recipe.
	for _, pkg := range []string{"./cmd/gt", "./plugins/polecat-image", "./cmd/sweep-run"} {
		env := envOfCall(t, w, pkg)
		for _, want := range []string{"GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0"} {
			if !contains(env, want) {
				t.Errorf("%s is built without %s in its environment: %v", pkg, want, env)
			}
		}
	}
	if res.GoFloor != "1.10.0" {
		t.Errorf("GoFloor = %q, want 1.10.0", res.GoFloor)
	}
}

// TestNoEngineCommandIsIssued checks no recorded call names a container engine,
// and that a seeded pin behind main with a module gap is refused by the
// pin-drift guard before any build runs.
func TestNoEngineCommandIsIssued(t *testing.T) {
	t.Parallel()
	t.Run("a successful build issues no engine command", func(t *testing.T) {
		w := newWorld(t)
		if _, err := Build(context.Background(), w.inputs(t.TempDir()), w.seams()); err != nil {
			t.Fatalf("Build: %v", err)
		}
		assertNoEngine(t, w.calls, w.goCalls)
	})

	t.Run("a pin behind main with a module gap is refused before any build", func(t *testing.T) {
		w := newWorld(t)
		// devops' main now requires a module devopsPin does not.
		w.commits[devopsNew] = map[string]string{
			"go.mod": goMod("github.com/steveyegge/devops", "1.8.0", "example.invalid/dep v1.0.0", "example.invalid/new v1.0.0"),
		}
		w.mains[devopsURL] = devopsNew
		_, err := Build(context.Background(), w.inputs(t.TempDir()), w.seams())
		if err == nil || !strings.Contains(err.Error(), "lacks modules") || !strings.Contains(err.Error(), "example.invalid/new") {
			t.Fatalf("Build error = %v, want the module-gap refusal naming example.invalid/new", err)
		}
		if len(w.goCalls) != 0 {
			t.Errorf("a refused pin still ran a build: %v", w.goCalls)
		}
		assertNoEngine(t, w.calls, w.goCalls)
	})
}

// TestContextRefusesBadInputs checks each refusal the build makes before any
// build runs: an input that is malformed, a tree that is not at --rev, a seed
// URL that does not pair with the seed pins, a URL with a credential, and a main
// that moves between its read and its fetch.
func TestContextRefusesBadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(w *world, in *Inputs)
		want   string
	}{
		{"a rev that is not a full sha", func(_ *world, in *Inputs) { in.Rev = "abc123" }, "not a full 40-character sha"},
		{"an out that is not absolute", func(_ *world, in *Inputs) { in.Out = "relative/out" }, "is not absolute"},
		{"a seed pin that is not a full sha", func(_ *world, in *Inputs) {
			in.Seeds[1].Commit = "abc123"
		}, "is not a full 40-character sha"},
		{"a seeded rig with no URL", func(_ *world, in *Inputs) {
			delete(in.SeedURLs, "devops")
		}, "names no URL for seeded rig devops"},
		{"a URL for a rig seed-pins does not seed", func(_ *world, in *Inputs) {
			in.SeedURLs["om"] = "http://fx.invalid/om.git"
		}, "which seed-pins does not seed"},
		{"a URL for the build rig", func(_ *world, in *Inputs) {
			in.SeedURLs[BuildRig] = "http://fx.invalid/gastown.git"
		}, "the build rig, which is seeded from --rev"},
		{"a checkout whose HEAD is not --rev", func(w *world, _ *Inputs) {
			w.head = bdPin
		}, "not --rev"},
		{"a bd URL that carries a credential", func(_ *world, in *Inputs) {
			in.BD.URL = "http://user:secret@fx.invalid/beads.git"
		}, "carries a credential"},
		{"a main that moves between its read and its fetch", func(w *world, _ *Inputs) {
			// devops' main reads as devopsNew, which differs from its pin, so it
			// is fetched; the next ls-remote answer is replaced by beadsMain.
			w.mains[devopsURL] = devopsNew
			w.commits[devopsNew] = w.commits[devopsPin]
			w.moveAfterLsRemote[devopsURL] = beadsMain
		}, "moved from"},
		{"a rig name that is a path", func(_ *world, in *Inputs) {
			in.Seeds[1].Rig = "../devops"
		}, "not a plain rig name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			in := w.inputs(t.TempDir())
			tc.mutate(w, &in)
			_, err := Build(context.Background(), in, w.seams())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want it to contain %q", err, tc.want)
			}
			if strings.Contains(fmt.Sprint(err), "secret") {
				t.Errorf("the refusal echoes a credential: %v", err)
			}
			if len(w.goCalls) != 0 {
				t.Errorf("a refused input still ran a build: %v", w.goCalls)
			}
		})
	}

	t.Run("a bd pin that resolves to another commit is refused once bd is built", func(t *testing.T) {
		w := newWorld(t)
		w.resolve[bdPin] = devopsNew
		w.commits[devopsNew] = w.commits[devopsPin]
		_, err := Build(context.Background(), w.inputs(t.TempDir()), w.seams())
		if err == nil || !strings.Contains(err.Error(), "not its pin") {
			t.Fatalf("Build error = %v, want the bd commit refusal", err)
		}
		if len(w.goCalls) != 1 || !strings.Contains(w.goCalls[0], "-o") {
			t.Errorf("want bd's compile as the only build before the refusal, got %v", w.goCalls)
		}
	})

	t.Run("an out directory that is not empty", func(t *testing.T) {
		w := newWorld(t)
		out := t.TempDir()
		if err := os.WriteFile(filepath.Join(out, "left-over"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Build(context.Background(), w.inputs(out), w.seams())
		if err == nil || !strings.Contains(err.Error(), "needs an empty directory") {
			t.Fatalf("Build error = %v, want the non-empty refusal", err)
		}
	})
}

// TestParseSeedURLs checks the seed-urls file: a line per rig, comments skipped,
// and a refusal for a malformed line or a rig named twice.
func TestParseSeedURLs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr string
	}{
		{"two rigs with a comment", "# c\ndevops http://a.invalid/d.git\n\nbeads http://a.invalid/b.git\n",
			map[string]string{"devops": "http://a.invalid/d.git", "beads": "http://a.invalid/b.git"}, ""},
		{"a line with no URL", "devops\n", nil, "want \"<rig> <url>\""},
		{"a rig named twice", "devops http://a.invalid/d.git\ndevops http://a.invalid/e.git\n", nil, "rig devops is named twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSeedURLs([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseSeedURLs error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSeedURLs: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("ParseSeedURLs = %v, want %v", got, tc.want)
			}
		})
	}
}

// assertNoEngine fails when a recorded call names a container engine, whether as
// the program run or as an argument.
func assertNoEngine(t *testing.T, calls, goCalls []string) {
	t.Helper()
	for _, c := range append(append([]string{}, calls...), goCalls...) {
		for _, e := range engines {
			if strings.Contains(" "+c+" ", " "+e+" ") {
				t.Errorf("a call names the engine %q: %s", e, c)
			}
		}
	}
	if len(calls) == 0 {
		t.Fatal("no call was recorded, so nothing proves the build ran")
	}
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
