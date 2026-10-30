package bumpversion

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// localGTPath is where make install puts gt for the fake home directory.
const localGTPath = "/home/op/.local/bin/gt"

// world is the fixture the run tests drive the command through: an in-memory tree
// holding the 1.2.1 fixtures, and fake git, make, nix, go and gt seams. Every git and
// process call is appended to log in call order, so a test can check the order of the
// steps that change the release.
type world struct {
	t          *testing.T
	files      *memFS
	gitCodes   map[string]int // exit status by git subcommand, 0 for any other
	gitArgs    [][]string     // the arguments of each git call, in order
	log        []string       // "make install", "git commit", ... in call order
	nix        bool           // nix is on PATH
	home       string         // the home directory the install check looks under
	homeErr    error          // why home is unknown, when it is
	failVendor bool           // go mod vendor fails, on both attempts
	nixHash    string         // what nix hash path prints
	makeErr    error          // what make install returns
	gtOut      string         // what the installed gt prints for its version
	localGT    bool           // make install left an executable gt in the home directory
	diffStat   string         // what git diff --stat prints
	diffErr    error          // what git diff --stat returns, when it cannot run
	readErr    map[string]error
	writeErr   map[string]error
	out        bytes.Buffer
}

// faultFS is the tree with the named reads and writes failing, so a test can drive
// each error branch of the run over the same files the run edits.
type faultFS struct {
	FS
	readErr  map[string]error
	writeErr map[string]error
}

func (f faultFS) ReadFile(name string) ([]byte, error) {
	if err := f.readErr[name]; err != nil {
		return nil, err
	}
	return f.FS.ReadFile(name)
}

func (f faultFS) WriteFile(name string, data []byte) error {
	if err := f.writeErr[name]; err != nil {
		return err
	}
	return f.FS.WriteFile(name, data)
}

// newWorld returns the tree at version 1.2.1, with nix on PATH and every seam
// succeeding.
func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{
		t: t,
		files: &memFS{files: map[string]string{
			VersionGoPath:   fixture(t, "version.go.before"),
			PackageJSONPath: fixture(t, "package.json.before"),
			FlakePath:       fixture(t, "flake.nix.before"),
			ChangelogPath:   fixture(t, "CHANGELOG.md.before"),
		}},
		gitCodes: map[string]int{},
		nix:      true,
		home:     "/home/op",
		nixHash:  newVendorHash,
		localGT:  true,
	}
}

// env is the run's seams, wired to the fakes. The date and home are fixed, so the
// CHANGELOG entry and the install path are the same on every machine.
func (w *world) env() Env {
	return Env{
		FS:  faultFS{FS: w.files, readErr: w.readErr, writeErr: w.writeErr},
		Git: Git{Run: w.runGit},
		Exec: Exec{
			Run:       w.runProcess,
			LookPath:  w.lookPath,
			MkdirTemp: func(_, pattern string) (string, error) { return os.MkdirTemp(w.t.TempDir(), pattern) },
		},
		Root:       "/root",
		Home:       w.home,
		HomeErr:    w.homeErr,
		Date:       "2026-10-30",
		Executable: func(path string) bool { return w.localGT && path == localGTPath },
		Out:        &w.out,
	}
}

func (w *world) run(opts Options) error {
	return Run(w.env(), opts)
}

func (w *world) runGit(dir string, args ...string) (int, error) {
	w.gitArgs = append(w.gitArgs, args)
	w.log = append(w.log, "git "+args[0])
	return w.gitCodes[args[0]], nil
}

func (w *world) runProcess(dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
	switch {
	case name == "git":
		w.log = append(w.log, "git "+args[0])
		return []byte(w.diffStat), nil, w.diffErr
	case name == "make":
		w.log = append(w.log, "make install")
		return nil, nil, w.makeErr
	case name == "go":
		w.log = append(w.log, "go mod vendor")
		if w.failVendor {
			return nil, nil, errors.New("go mod vendor failed")
		}
		return nil, nil, nil
	case name == "nix":
		w.log = append(w.log, "nix hash path")
		return []byte(w.nixHash), nil, nil
	case name == "gt" || name == localGTPath:
		w.log = append(w.log, "gt version")
		return []byte(w.gtOut), nil, nil
	}
	w.t.Fatalf("unexpected process %q", name)
	return nil, nil, nil
}

func (w *world) lookPath(file string) (string, error) {
	if !w.nix {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + file, nil
}

// steps returns the recorded calls that change the release: the install, the staging
// of the commit, the commit and the tag.
func (w *world) steps() []string {
	var got []string
	for _, c := range w.log {
		switch c {
		case "make install", "git add", "git commit", "git tag":
			got = append(got, c)
		}
	}
	return got
}

// gitArgsOf returns the arguments of the first git call whose subcommand is sub, or nil.
func (w *world) gitArgsOf(sub string) []string {
	for _, args := range w.gitArgs {
		if args[0] == sub {
			return args
		}
	}
	return nil
}

// goldenAfter returns the fixture the tree should hold after a 1.2.1 to 1.3.0 bump
// that has the given hash in flake.nix.
func goldenAfter(t *testing.T, hash string) map[string]string {
	t.Helper()
	flake := strings.ReplaceAll(fixture(t, "flake.nix.after"), oldVendorHash, hash)
	return map[string]string{
		VersionGoPath:   fixture(t, "version.go.after"),
		PackageJSONPath: fixture(t, "package.json.after"),
		FlakePath:       flake,
		ChangelogPath:   fixture(t, "CHANGELOG.md.after"),
	}
}

// TestRunNoFlagsEditsAllFilesAndShowsReview: with no flags, the four version files
// hold 1.3.0 afterwards, the run verifies them, makes no commit and prints the review
// hint with the command line to release.
func TestRunNoFlagsEditsAllFilesAndShowsReview(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if err := w.run(Options{Version: "1.3.0"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, w.out.String())
	}
	for name, want := range goldenAfter(t, newVendorHash) {
		if got := w.files.files[name]; got != want {
			t.Errorf("%s after the run:\n%s\nwant:\n%s", name, got, want)
		}
	}
	out := w.out.String()
	for _, want := range []string{
		"✓ All versions match: 1.3.0",
		"Review the changes above.",
		"  bump-version 1.3.0 --commit --tag --install\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if w.gitArgsOf("commit") != nil {
		t.Errorf("a run with no flags made a commit: %v", w.gitArgs)
	}
}

// TestRunCommitTagInstallOrder: the install runs first, then the commit, then the tag,
// as the fakes recorded them, and the verification of the installed gt is printed.
func TestRunCommitTagInstallOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.gtOut = "gt version 1.3.0\n"
	opts := Options{Version: "1.3.0", Commit: true, Tag: true, Install: true}
	if err := w.run(opts); err != nil {
		t.Fatalf("Run(%+v): %v\n%s", opts, err, w.out.String())
	}
	want := []string{"make install", "git add", "git commit", "git tag"}
	if got := w.steps(); !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q, want %q", got, want)
	}
	if out := w.out.String(); !strings.Contains(out, "✓ Verified: gt version 1.3.0") || !strings.Contains(out, "git push origin main") {
		t.Errorf("output lacks the verified line or the next steps:\n%s", out)
	}
}

// TestRunFailureStopsEarly: a failing vendorHash and a version that does not match
// after the edits each fail the run before the install and before any git step.
func TestRunFailureStopsEarly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(w *world)
		wantErr string
	}{
		{
			name:    "vendorHash fails",
			setup:   func(w *world) { w.failVendor = true },
			wantErr: "failed to update vendorHash: go mod vendor",
		},
		{
			// The package.json line has no space after the colon, so the edit does not
			// match and the file still holds 1.2.1 when Verify reads it back.
			name: "mismatch after editing",
			setup: func(w *world) {
				w.files.files[PackageJSONPath] = `{"version":"1.2.1"}`
			},
			wantErr: "Version mismatch detected!",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			tc.setup(w)
			err := w.run(Options{Version: "1.3.0", Commit: true, Tag: true, Install: true})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want it to contain %q", err, tc.wantErr)
			}
			if got := w.steps(); len(got) != 0 {
				t.Errorf("steps after a failed run = %q, want none", got)
			}
		})
	}
}

// TestRunNixAbsentSkipsFlakeAndStillVerifies: with no nix on PATH, flake.nix is neither
// read nor written, the run says so, verification passes on the two files that remain,
// and the commit stages no flake.nix.
func TestRunNixAbsentSkipsFlakeAndStillVerifies(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.nix = false
	flakeBefore := w.files.files[FlakePath]
	if err := w.run(Options{Version: "1.3.0", Commit: true}); err != nil {
		t.Fatalf("Run: %v\n%s", err, w.out.String())
	}
	if got := w.files.files[FlakePath]; got != flakeBefore {
		t.Errorf("flake.nix changed with nix absent:\n%s", got)
	}
	for _, call := range w.log {
		if call == "nix hash path" || call == "go mod vendor" {
			t.Errorf("nix absent but the run ran %q", call)
		}
	}
	out := w.out.String()
	if !strings.Contains(out, "  • "+NixSkippedNote) || !strings.Contains(out, "✓ All versions match: 1.3.0") {
		t.Errorf("output lacks the skip line or the verification:\n%s", out)
	}
	want := []string{"add", VersionGoPath, PackageJSONPath, ChangelogPath}
	if got := w.gitArgsOf("add"); !reflect.DeepEqual(got, want) {
		t.Errorf("git add args = %q, want %q", got, want)
	}
}

// TestRunDirtyTreeRefusesBeforeEditing: a dirty tree under --commit is refused, a dirty
// tree without --commit is refused unless --allow-dirty is given, and a git that fails
// for another reason stops the run. None of the refusals edits a file.
func TestRunDirtyTreeRefusesBeforeEditing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		codes      map[string]int
		opts       Options
		wantErr    string
		wantOut    string
		wantEdited bool
	}{
		{
			name:    "dirty with --commit",
			codes:   map[string]int{"diff-index": 1},
			opts:    Options{Version: "1.3.0", Commit: true, AllowDirty: true},
			wantErr: "Error: Cannot auto-commit with existing uncommitted changes",
			wantOut: "Warning: You have uncommitted changes",
		},
		{
			name:    "dirty without --allow-dirty",
			codes:   map[string]int{"diff-index": 1},
			opts:    Options{Version: "1.3.0"},
			wantErr: "uncommitted changes (pass --allow-dirty",
			wantOut: "Warning: You have uncommitted changes",
		},
		{
			name:    "git fails for another reason",
			codes:   map[string]int{"diff-index": 128},
			opts:    Options{Version: "1.3.0", AllowDirty: true},
			wantErr: "git diff-index: exit 128",
		},
		{
			name:       "dirty with --allow-dirty continues",
			codes:      map[string]int{"diff-index": 1},
			opts:       Options{Version: "1.3.0", AllowDirty: true},
			wantOut:    "Warning: You have uncommitted changes",
			wantEdited: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.gitCodes = tc.codes
			before := w.files.files[VersionGoPath]
			err := w.run(tc.opts)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Run: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Run error = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.wantOut != "" && !strings.Contains(w.out.String(), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, w.out.String())
			}
			if strings.Contains(w.out.String(), "git checkout") {
				t.Errorf("a refusal before any edit named files to restore:\n%s", w.out.String())
			}
			edited := w.files.files[VersionGoPath] != before
			if edited != tc.wantEdited {
				t.Errorf("version.go edited = %v, want %v", edited, tc.wantEdited)
			}
		})
	}
}

// TestRunInstallVerifiesTheInstalledVersion: the installed gt is checked after
// make install. A match is verified, a different version is a warning that exits 0, a
// gt with no version line is unverified, and a failed make install stops the run before
// any git step.
func TestRunInstallVerifiesTheInstalledVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gtOut   string
		makeErr error
		wantOut string
		wantErr string
	}{
		{name: "match", gtOut: "gt version 1.3.0\n", wantOut: "✓ Verified: gt version 1.3.0"},
		{name: "different version", gtOut: "gt version 1.2.0\n", wantOut: "⚠ gt reports 1.2.0 (expected 1.3.0)"},
		{name: "no version line", gtOut: "usage: gt\n", wantOut: "⚠ cannot verify the installed gt: gt version printed no version"},
		{name: "make install fails", makeErr: errors.New("exit 2"), wantErr: "make install failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.gtOut = tc.gtOut
			w.makeErr = tc.makeErr
			err := w.run(Options{Version: "1.3.0", Commit: true, Install: true})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Run error = %v, want it to contain %q", err, tc.wantErr)
				}
				if got := w.steps(); !reflect.DeepEqual(got, []string{"make install"}) {
					t.Errorf("steps = %q, want only the failed install", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v\n%s", err, w.out.String())
			}
			if !strings.Contains(w.out.String(), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, w.out.String())
			}
		})
	}
}

// TestRunRefusesOutsideTheRepository: a directory without internal/cmd/version.go, or
// a version.go with no Version line, is refused before anything is edited.
func TestRunRefusesOutsideTheRepository(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(w *world)
		wantErr string
	}{
		{
			name:    "no version.go",
			setup:   func(w *world) { delete(w.files.files, VersionGoPath) },
			wantErr: "Error: Must run from repository root",
		},
		{
			name:    "no Version line",
			setup:   func(w *world) { w.files.files[VersionGoPath] = "package cmd\n" },
			wantErr: "Error: no Version = line in internal/cmd/version.go",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			tc.setup(w)
			before := len(w.files.files)
			err := w.run(Options{Version: "1.3.0"})
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Run error = %v, want %q", err, tc.wantErr)
			}
			if strings.Contains(w.out.String(), "git checkout") {
				t.Errorf("a refused run named files to restore:\n%s", w.out.String())
			}
			if len(w.files.files) != before || len(w.log) != 0 {
				t.Errorf("refused run touched the tree or ran a process: log %q", w.log)
			}
		})
	}
}

// TestRunIOErrorsStopBeforeTheGitSteps: a read or write of a version file that fails,
// and a git diff --stat that cannot run, each stop the run with that error. No install
// and no git step after the edits runs.
func TestRunIOErrorsStopBeforeTheGitSteps(t *testing.T) {
	t.Parallel()
	failure := errors.New("disk on fire")
	tests := []struct {
		name  string
		setup func(w *world)
	}{
		{name: "version.go unreadable", setup: func(w *world) { w.readErr = map[string]error{VersionGoPath: failure} }},
		{name: "version.go unwritable", setup: func(w *world) { w.writeErr = map[string]error{VersionGoPath: failure} }},
		{name: "package.json unwritable", setup: func(w *world) { w.writeErr = map[string]error{PackageJSONPath: failure} }},
		{name: "flake.nix unwritable", setup: func(w *world) { w.writeErr = map[string]error{FlakePath: failure} }},
		{name: "CHANGELOG.md unwritable", setup: func(w *world) { w.writeErr = map[string]error{ChangelogPath: failure} }},
		{name: "git diff --stat cannot run", setup: func(w *world) { w.diffErr = failure }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			tc.setup(w)
			err := w.run(Options{Version: "1.3.0", Commit: true, Tag: true, Install: true})
			if err == nil || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("Run error = %v, want it to contain %q", err, failure)
			}
			if got := w.steps(); len(got) != 0 {
				t.Errorf("steps after a failed run = %q, want none", got)
			}
		})
	}
}

// summaryOf returns the block the command prints last, from its last "Summary:" header
// to the end of the output.
func summaryOf(t *testing.T, out string) string {
	t.Helper()
	i := strings.LastIndex(out, "Summary:\n")
	if i < 0 {
		t.Fatalf("output has no summary block:\n%s", out)
	}
	return out[i:]
}

// TestRunSummaryRepeatsTheInstallWarning: the summary is the last block the command
// prints, so an install warning is the final thing the operator reads. A mismatch or an
// unverified gt is repeated there and the run still commits and tags; a verified gt
// leaves no warning in it.
func TestRunSummaryRepeatsTheInstallWarning(t *testing.T) {
	t.Parallel()
	release := []string{"make install", "git add", "git commit", "git tag"}
	tests := []struct {
		name  string
		gtOut string
		opts  Options
		want  string
		steps []string
	}{
		{
			name:  "mismatch still commits and tags",
			gtOut: "gt version 1.2.0\n",
			opts:  Options{Version: "1.3.0", Commit: true, Tag: true, Install: true},
			want:  "Summary:\n⚠ installed gt reports 1.2.0, wanted 1.3.0\nCommitted and tagged v1.3.0.\n",
			steps: release,
		},
		{
			name:  "unverified still commits and tags",
			gtOut: "usage: gt\n",
			opts:  Options{Version: "1.3.0", Commit: true, Tag: true, Install: true},
			want:  "Summary:\n⚠ installed gt unverified (gt version printed no version), wanted 1.3.0\nCommitted and tagged v1.3.0.\n",
			steps: release,
		},
		{
			name:  "verified leaves no warning",
			gtOut: "gt version 1.3.0\n",
			opts:  Options{Version: "1.3.0", Commit: true, Tag: true, Install: true},
			want:  "Summary:\nCommitted and tagged v1.3.0.\n",
			steps: release,
		},
		{
			name:  "mismatch without a commit says none was made",
			gtOut: "gt version 1.2.0\n",
			opts:  Options{Version: "1.3.0", Install: true},
			want:  "Summary:\n⚠ installed gt reports 1.2.0, wanted 1.3.0\nNo commit or tag made.\n",
			steps: []string{"make install"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.gtOut = tc.gtOut
			if err := w.run(tc.opts); err != nil {
				t.Fatalf("Run(%+v): %v\n%s", tc.opts, err, w.out.String())
			}
			if got := summaryOf(t, w.out.String()); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
			if got := w.steps(); !reflect.DeepEqual(got, tc.steps) {
				t.Errorf("steps = %q, want %q", got, tc.steps)
			}
		})
	}
}

// TestRunFailureAfterEditsNamesTheFilesToRestore: a run that fails after it has edited
// files prints one line naming them and the git checkout that restores them. The files
// are the ones the run wrote, in order: the flake.nix version edit landed before the
// vendorHash failed.
func TestRunFailureAfterEditsNamesTheFilesToRestore(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.failVendor = true
	if err := w.run(Options{Version: "1.3.0"}); err == nil {
		t.Fatal("Run succeeded with a failing vendorHash")
	}
	want := "Edited before this failure. Restore with: git checkout -- " +
		VersionGoPath + " " + PackageJSONPath + " " + FlakePath + "\n"
	if !strings.Contains(w.out.String(), want) {
		t.Errorf("output lacks the restore line %q:\n%s", want, w.out.String())
	}
}

// TestRunCommitFailureStopsBeforeTheTagAndRestoresTheEdits: a git commit that exits
// non-zero stops the run before the tag, returns the commit's status, and prints the
// restore line for the four files the run edited.
func TestRunCommitFailureStopsBeforeTheTagAndRestoresTheEdits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.gitCodes = map[string]int{"commit": 1}
	err := w.run(Options{Version: "1.3.0", Commit: true, Tag: true})
	if err == nil || !strings.Contains(err.Error(), "git commit: exit 1") {
		t.Fatalf("Run error = %v, want it to contain %q", err, "git commit: exit 1")
	}
	if got := w.steps(); !reflect.DeepEqual(got, []string{"git add", "git commit"}) {
		t.Errorf("steps = %q, want the staging and the failed commit only", got)
	}
	want := "Edited before this failure. Restore with: git checkout -- " +
		VersionGoPath + " " + PackageJSONPath + " " + FlakePath + " " + ChangelogPath + "\n"
	if !strings.Contains(w.out.String(), want) {
		t.Errorf("output lacks the restore line %q:\n%s", want, w.out.String())
	}
}

// TestRunInstallWithUnknownHomeIsUnverifiedAndRunsNoGT: when the home directory cannot
// be resolved, the install check reports unverified with that reason and runs no gt. A
// gt that would verify is on hand, so a run that looked for gt anyway would show it.
func TestRunInstallWithUnknownHomeIsUnverifiedAndRunsNoGT(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.home = ""
	w.homeErr = errors.New("$HOME is not set")
	w.gtOut = "gt version 1.3.0\n"
	if err := w.run(Options{Version: "1.3.0", Commit: true, Install: true}); err != nil {
		t.Fatalf("Run: %v\n%s", err, w.out.String())
	}
	want := "⚠ cannot verify the installed gt: home directory unknown: $HOME is not set"
	if !strings.Contains(w.out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, w.out.String())
	}
	for _, call := range w.log {
		if call == "gt version" {
			t.Errorf("an unknown home ran gt: log %q", w.log)
		}
	}
}

// TestRunWithoutChangelogWarnsAndContinues: a repository with no CHANGELOG.md is still
// bumped, the run says the file was skipped, and no CHANGELOG.md is created.
func TestRunWithoutChangelogWarnsAndContinues(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	delete(w.files.files, ChangelogPath)
	if err := w.run(Options{Version: "1.3.0"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, w.out.String())
	}
	if _, ok := w.files.files[ChangelogPath]; ok {
		t.Errorf("run created %s", ChangelogPath)
	}
	if !strings.Contains(w.out.String(), "Warning: CHANGELOG.md not found, skipping") {
		t.Errorf("output lacks the skip warning:\n%s", w.out.String())
	}
}
