package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/installer"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// The commits a fake run's checkout answers with: the one the installed binary
// was built from, and the one being installed.
const (
	prevSHA = "c1111111"
	fullSHA = "c2222222"
)

// testEnv is a process whose install tree is a temporary directory and which
// has no town above its working directory, so a run never reads or writes the
// host's install directory.
func testEnv(t *testing.T) (environment, string) {
	t.Helper()
	env, root, _ := newTestEnv(t)
	return env, root
}

// newTestEnv is testEnv with the run's seams, for a test that drives steps the
// others never reach.
func newTestEnv(t *testing.T) (environment, string, *fakeRun) {
	t.Helper()
	root := t.TempDir()
	f := &fakeRun{}
	values := map[string]string{
		"INSTALL_GT_BIN_DIR":    filepath.Join(root, "bin"),
		"INSTALL_GT_DAEMON_DIR": filepath.Join(root, "daemon"),
		"INSTALL_GT_RIG_DIR":    filepath.Join(root, "rig"),
		"INSTALL_GT_SIGN_DIR":   filepath.Join(root, "signing"),
	}
	env := environment{
		getenv: func(key string) string { return values[key] },
		getwd:  func() (string, error) { return root, nil },
		home:   func() (string, error) { return root, nil },
		exe:    func() (string, error) { return filepath.Join(root, "gt-installer"), nil },
		newRun: f.newRun,
	}
	return env, root, f
}

// fakeRun is the run a test's environment builds: every external command is
// recorded and none is started, the checkout answers for itself, and the run
// moves files in the temporary tree the test made. It is what keeps this
// package's tests off make, gt and codesign (gt-tpg0p.11).
type fakeRun struct {
	// installed is set once the install step has put a binary in place, so the
	// commit the binary reports moves from the replaced commit to the new one,
	// as a real install's does.
	installed bool
	// bin is the installed binary the install step writes.
	bin string
	// commands is the argv of every command the steps ran through the run's
	// command seam, stdins the standard input each was fed, and execs those the
	// exit contract ran.
	commands [][]string
	stdins   []string
	execs    [][]string
	// imageBuilds is every sandbox image build the flow asked for, and
	// imageAfterSwap is set when one was asked for after install-local had
	// already put the binary in place.
	imageBuilds    []installer.ImageBuild
	imageAfterSwap bool
	// imageCode is the status the image build ends with.
	imageCode int
}

func (f *fakeRun) newRun(cfg installer.Config, c *installer.Contract, paths installer.Paths) *installer.Run {
	f.bin = paths.GT()
	c.Exec = func(name string, args ...string) ([]byte, error) {
		f.execs = append(f.execs, append([]string{name}, args...))
		return nil, nil
	}
	run := &installer.Run{
		Cfg:   cfg,
		C:     c,
		Paths: paths,
		Repo: installer.Repo{
			Fetch:      func() error { return nil },
			Resolve:    resolved,
			CommitTime: func(string) (time.Time, error) { return time.Unix(0, 0).UTC(), nil },
			IsAncestor: func(ancestor, descendant string) bool {
				return ancestor == descendant || (ancestor == prevSHA && descendant == fullSHA)
			},
			Dirty:       func() (bool, error) { return false, nil },
			Branch:      func() string { return "main" },
			FastForward: func(string) error { return nil },
			Head:        func() (string, error) { return fullSHA, nil },
			AheadOf:     func(string) (int, error) { return 0, nil },
		},
		Files: installer.Files{
			// chflags and chattr are programs; the flag they set is not what
			// these tests watch.
			ClearImmutable: func(string) {},
			SetImmutable:   func(string) {},
			// The sandbox image step's build output is answered rather than
			// created: a unit test writes no executable file (gt-tpg0p.12).
			// Everything else is the real check.
			IsExecutable: func(path string) bool {
				if path == filepath.Join(paths.RigDir, "gt") {
					return true
				}
				info, err := os.Stat(path)
				return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
			},
		},
		Bin: installer.Binary{Commit: func(string, string) string {
			if f.installed {
				return fullSHA
			}
			return prevSHA
		}},
		// Neither timeout(1) nor gtimeout is on the fake PATH, so codesign runs
		// unwrapped, as it does where neither is installed.
		Found: func(string) bool { return false },
	}
	run.Cmd = func(dir, name string, stdin []byte, args ...string) ([]byte, []byte, int) {
		f.commands = append(f.commands, append([]string{name}, args...))
		f.stdins = append(f.stdins, string(stdin))
		return f.respond(name, args)
	}
	// The image build is recorded, never started: the step's preflight reads
	// real files under the temporary root and the build itself is the seam
	// (gt-tpg0p.12).
	run.ImageRunner = func(b installer.ImageBuild) (int, error) {
		if f.installed {
			f.imageAfterSwap = true
		}
		f.imageBuilds = append(f.imageBuilds, b)
		return f.imageCode, nil
	}
	return run
}

// respond answers the commands the flow runs: a make install-local puts the
// binary in place, as the real one does, and the stale read answers the commit
// that binary was built from. A command none of the steps makes fails, so a
// step that starts running something new is a failed run rather than a pass.
func (f *fakeRun) respond(name string, args []string) ([]byte, []byte, int) {
	switch {
	case name == "make" && len(args) > 0 && args[len(args)-1] == "install-local":
		if err := os.WriteFile(f.bin, []byte("installed binary"), 0o644); err != nil {
			return []byte(err.Error()), nil, 1
		}
		f.installed = true
		return nil, nil, 0
	case name == "make", name == "security", name == "codesign":
		return nil, nil, 0
	case name == f.bin && len(args) > 0 && args[0] == "stale":
		return []byte(`{"binary_commit": "` + fullSHA + `"}`), nil, 0
	case name == f.bin:
		return nil, nil, 0
	}
	return []byte("unexpected command " + name + "\n"), nil, 1
}

// resolved is the fake checkout's answer for a ref.
func resolved(ref string) string {
	switch ref {
	case "origin/main", "HEAD", fullSHA:
		return fullSHA
	case prevSHA:
		return prevSHA
	}
	return ""
}

// A bad argument is reported before any directory is resolved: the message on
// stderr, then the guard's result line, status 1.
func TestRunWithABadArgument(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		args       []string
		wantLast   string
		wantStderr string
	}{
		{
			name:       "an empty --sha names nothing",
			args:       []string{"--sha", ""},
			wantLast:   "install-gt: RESULT failed - - unexpected",
			wantStderr: "install-gt: --sha needs a commit",
		},
		{
			name:       "a bad source still ends in a result line",
			args:       []string{"--sha", "abc123", "--source", "bogus"},
			wantLast:   "install-gt: RESULT failed abc123 - unexpected",
			wantStderr: "install-gt: --source must be manual, rebuild-gt or post-land",
		},
		{
			name:       "an unknown argument stops parsing where it stands",
			args:       []string{"--bogus", "--sha", "abc123"},
			wantLast:   "install-gt: RESULT failed origin/main - unexpected",
			wantStderr: "install-gt: unknown argument: --bogus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := testEnv(t)
			var out, errOut strings.Builder

			code := runWith(tc.args, &out, &errOut, env)

			if code != 1 {
				t.Errorf("run(%q) = %d, want 1", tc.args, code)
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != 1 || lines[0] != tc.wantLast {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantLast)
			}
			if got := strings.TrimSuffix(errOut.String(), "\n"); got != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tc.wantStderr)
			}
		})
	}
}

// The first section reports for itself: no build checkout is a failed run with
// the script's log line, result line and receipt, and the binary is untouched.
func TestRunWithoutABuildCheckout(t *testing.T) {
	t.Parallel()
	env, root := testEnv(t)
	var out, errOut strings.Builder

	code := runWith(nil, &out, &errOut, env)

	if code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	rig := filepath.Join(root, "rig")
	want := "[install-gt] No build checkout at " + rig + "\n" +
		"install-gt: RESULT failed origin/main - no-rig\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
	receipts, err := os.ReadFile(filepath.Join(root, "daemon", "install-receipts.jsonl"))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	if !strings.Contains(string(receipts), `"reason": "no-rig"`) {
		t.Errorf("receipts = %q, want reason no-rig", receipts)
	}
}

// --dry-run plans the gateway and changes nothing, not even a receipt: it
// states its own result and exits 0 (gt-tpg0p.9).
func TestRunDryRunTouchesNothing(t *testing.T) {
	t.Parallel()
	env, root := testEnv(t)
	if err := os.MkdirAll(filepath.Join(root, "rig", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	var out, errOut strings.Builder

	code := runWith([]string{"--dry-run"}, &out, &errOut, env)

	if code != 0 {
		t.Errorf("run(--dry-run) = %d, want 0", code)
	}
	want := "[install-gt] gateway: skipped, no " +
		filepath.Join(root, ".config", "openshell", "gateway.toml") + "\n" +
		"install-gt: RESULT dry-run origin/main - -\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
	if after := snapshot(t, root); !reflect.DeepEqual(after, before) {
		t.Errorf("the tree changed:\nbefore %v\nafter  %v", before, after)
	}
}

// A program that cannot name its own directory has no gateway template to fall
// back on: the step warns about the checkout's missing template and the run
// still reports its dry-run (gt-tpg0p.9).
func TestRunDryRunWithoutAProgramDirectory(t *testing.T) {
	t.Parallel()
	env, root := testEnv(t)
	env.exe = func() (string, error) { return "", fs.ErrNotExist }
	toml := filepath.Join(root, ".config", "openshell", "gateway.toml")
	if err := os.MkdirAll(filepath.Dir(toml), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toml, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	var out, errOut strings.Builder

	code := runWith([]string{"--dry-run"}, &out, &errOut, env)

	if code != 0 {
		t.Errorf("run(--dry-run) = %d, want 0", code)
	}
	if want := "install-gt: RESULT dry-run origin/main - -\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	tmpl := filepath.Join(root, "rig", "scripts", "launchd", "com.gastown.openshell-gateway.plist.tmpl")
	if want := "[install-gt] WARNING: gateway: no template at " + tmpl + "\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
	if after := snapshot(t, root); !reflect.DeepEqual(after, before) {
		t.Errorf("the tree changed:\nbefore %v\nafter  %v", before, after)
	}
}

// TestRunInstallsEndToEnd drives every step of a run — the target, the
// refusals, the build, the install, the signing, the smoke check, the syncs and
// the restart marker — with the run's commands recorded and none started. It is
// the case the run seam exists for: the flow reaches make, gt and codesign, and
// a test that let one of them through would run it on the host (gt-tpg0p.11).
func TestRunInstallsEndToEnd(t *testing.T) {
	t.Parallel()
	env, root, f := installedTown(t)
	binDir := filepath.Join(root, "bin")
	signDir := filepath.Join(root, "signing")
	keychain := filepath.Join(signDir, "gastown-signing.keychain-db")
	var out, errOut strings.Builder

	code := runWith(nil, &out, &errOut, env)

	if code != 0 {
		t.Fatalf("run() = %d, want 0\nstdout %q\nstderr %q", code, out.String(), errOut.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if want := "install-gt: RESULT installed " + fullSHA + " " + prevSHA + " -"; lines[len(lines)-1] != want {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], want)
	}
	// The sandbox image was built once, from the checkout's program and build
	// output, and the swap waited for it (gt-tpg0p.12).
	if len(f.imageBuilds) != 1 {
		t.Fatalf("image builds = %+v, want one", f.imageBuilds)
	}
	if image := f.imageBuilds[0]; image.Detached || f.imageAfterSwap {
		t.Errorf("image build = %+v, want it before the swap", image)
	} else if want := filepath.Join(root, "rig", "plugins", "polecat-image", "bin", "polecat-image"); image.Program != want {
		t.Errorf("image program = %q, want %q", image.Program, want)
	}
	gt := filepath.Join(binDir, "gt")
	// codesign's last argument is the mktemp copy beside the binary, whose name
	// nothing can predict, so the first rows are a prefix and that one row's
	// path is checked by its shape.
	prefix := [][]string{
		{"make", "SKIP_UPDATE_CHECK=1", "build"},
		{"make", "SKIP_UPDATE_CHECK=1", "INSTALL_DIR=" + binDir, "install-local"},
		{"security", "-i"},
		{"codesign", "--force", "--keychain", keychain, "-s", "gastown-dev-unattended", "-i", "com.gastown.gt"},
	}
	suffix := [][]string{
		{gt, "stale", "--json"},
		{gt, "formula", "sync"},
		{gt, "plugin", "sync"},
		{gt, "escalate", "clear", "--fingerprint", "install-gt:build-failed",
			"--fingerprint", "install-gt:smoke-failed", "--reason", "install-gt: " + fullSHA + " is in force"},
	}
	if want := len(prefix) + len(suffix); len(f.commands) != want {
		t.Fatalf("commands = %q, want %d of them", f.commands, want)
	}
	for i, want := range prefix {
		got := f.commands[i]
		if i == len(prefix)-1 {
			signed := got[len(want)]
			got = got[:len(want)]
			if dir, base := filepath.Dir(signed), filepath.Base(signed); dir != binDir || !strings.HasPrefix(base, ".gt.signed.") {
				t.Errorf("signed path = %q, want a .gt.signed.* copy beside %q", signed, gt)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("command %d = %q, want %q", i, got, want)
		}
	}
	for i, want := range suffix {
		if got := f.commands[len(prefix)+i]; !reflect.DeepEqual(got, want) {
			t.Errorf("command %d = %q, want %q", len(prefix)+i, got, want)
		}
	}
	// The keychain password reaches security on stdin, never in the argv the
	// process table shows while the unlock runs (gt-4ucnp).
	if got, line := f.stdins[2], "unlock-keychain -p test-password "; !strings.HasPrefix(got, line) || !strings.Contains(got, keychain) {
		t.Errorf("unlock stdin = %q, want it to start %q and name %q", got, line, keychain)
	}
	// Nothing ran through the exit contract either: no escalation and no
	// gateway, which this run skips for want of a gateway.toml.
	if len(f.execs) != 0 {
		t.Errorf("contract commands = %q, want none", f.execs)
	}
	// The installed binary is the one the install wrote, and the install is
	// recorded in the marker the daemon reads and the receipt it leaves.
	if got := readFile(t, gt); got != "installed binary" {
		t.Errorf("installed binary = %q, want the install step's", got)
	}
	marker := readFile(t, filepath.Join(root, "daemon", "restart-pending.json"))
	if !strings.Contains(marker, `"commit": "`+fullSHA+`"`) || !strings.Contains(marker, `"source": "manual"`) {
		t.Errorf("restart marker = %q, want the installed commit and its source", marker)
	}
	receipts := readFile(t, filepath.Join(root, "daemon", "install-receipts.jsonl"))
	if !strings.Contains(receipts, `"event": "installed"`) || !strings.Contains(receipts, `"commit": "`+fullSHA+`"`) {
		t.Errorf("receipts = %q, want the installed event for %s", receipts, fullSHA)
	}
}

// installedTown is a town a run can install into: the build checkout, the
// daemon directory, the signing material and the binary being replaced, under
// the temporary root newTestEnv made.
func installedTown(t *testing.T) (environment, string, *fakeRun) {
	t.Helper()
	env, root, f := newTestEnv(t)
	binDir, signDir := filepath.Join(root, "bin"), filepath.Join(root, "signing")
	for _, dir := range []string{filepath.Join(root, "rig", ".git"), filepath.Join(root, "daemon")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]struct {
		body string
		mode os.FileMode
	}{
		filepath.Join(signDir, "gastown-signing.keychain-db"): {"keychain", 0o600},
		filepath.Join(signDir, "keychain.pass"):               {"test-password\n", 0o600},
		filepath.Join(binDir, "gt"):                           {"old binary", 0o644},
		// The checkout's plugin program; the build output beside it is
		// answered executable by the run's file seam, since creating one would
		// be an executable file in a unit test (gt-tpg0p.12).
		filepath.Join(root, "rig", "plugins", "polecat-image", "bin", "polecat-image"): {"program", 0o644},
	}
	for path, file := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.body), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return env, root, f
}

// An image build that does not finish stops the install before the swap: the
// run ends failed/build-failed, install-local never runs, and no restart marker
// is written, so the town keeps the binary it has (gt-xzo8a, gt-tpg0p.12).
func TestRunFailsBeforeTheSwapWhenTheImageBuildFails(t *testing.T) {
	t.Parallel()
	env, root, f := installedTown(t)
	f.imageCode = 1
	var out, errOut strings.Builder

	code := runWith(nil, &out, &errOut, env)

	if code != 1 {
		t.Fatalf("run() = %d, want 1\nstdout %q\nstderr %q", code, out.String(), errOut.String())
	}
	for _, cmd := range f.commands {
		if len(cmd) > 0 && cmd[len(cmd)-1] == "install-local" {
			t.Fatalf("commands = %q, want no install-local after a failed image build", f.commands)
		}
	}
	if f.installed {
		t.Error("the binary was swapped after a failed image build")
	}
	if _, err := os.Stat(filepath.Join(root, "daemon", "restart-pending.json")); err == nil {
		t.Error("a restart marker was written after a failed image build")
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if want := "install-gt: RESULT failed " + fullSHA + " " + prevSHA + " build-failed"; lines[len(lines)-1] != want {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], want)
	}
}

// --no-image-first swaps the binary and writes its restart marker without
// waiting for the image, then rebuilds it detached: the run still reports
// installed, and the build was asked for after the swap (gt-vhn89).
func TestRunNoImageFirstBuildsTheImageAfterTheSwap(t *testing.T) {
	t.Parallel()
	env, root, f := installedTown(t)
	var out, errOut strings.Builder

	code := runWith([]string{"--no-image-first"}, &out, &errOut, env)

	if code != 0 {
		t.Fatalf("run(--no-image-first) = %d, want 0\nstdout %q\nstderr %q", code, out.String(), errOut.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if want := "install-gt: RESULT installed " + fullSHA + " " + prevSHA + " -"; lines[len(lines)-1] != want {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], want)
	}
	if len(f.imageBuilds) != 1 {
		t.Fatalf("image builds = %+v, want one", f.imageBuilds)
	}
	image := f.imageBuilds[0]
	if !image.Detached || !f.imageAfterSwap {
		t.Errorf("image build = %+v (after the swap: %v), want it detached after the swap", image, f.imageAfterSwap)
	}
	if image.Fingerprint != "install-gt:image-build-failed" || image.GT != filepath.Join(root, "bin", "gt") {
		t.Errorf("escalation = %q/%q, want the detached build's own", image.GT, image.Fingerprint)
	}
	if !strings.Contains(out.String(), "polecat image: rebuild for "+fullSHA+" started in the background") {
		t.Errorf("output lacks the detached start line:\n%s", out.String())
	}
}

// A detached image build's worker is not an install: its arguments go to the
// worker and the run writes no result line of its own (gt-tpg0p.12).
func TestRunRoutesTheImageWorker(t *testing.T) {
	t.Parallel()
	env, _ := testEnv(t)
	var got []string
	env.imageWorker = func(args []string, stderr io.Writer) int {
		got = args
		return 7
	}
	var out, errOut strings.Builder

	args := []string{installer.ImageWorkerFlag, "--alarm", "60", "--", "/rig/polecat-image"}
	if code := runWith(args, &out, &errOut, env); code != 7 {
		t.Errorf("run(%q) = %d, want the worker's 7", args, code)
	}
	if want := args[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("worker args = %q, want %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: a worker names no result line", out.String())
	}
}

// readFile is a file's contents, for a test that a step wrote.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// snapshot is every path under root, with a file's contents, so a test can say
// what a run changed.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return tree
}
