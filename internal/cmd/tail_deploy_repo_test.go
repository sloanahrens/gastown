package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/version"
)

// tailDeployTown makes a town root with a gastown source checkout under
// mayor/rig, the marker GetRepoRootForTown looks for, and no bare repo. The
// checkout is plain files: nothing here runs git.
func tailDeployTown(t *testing.T) (townRoot, checkout string) {
	t.Helper()
	townRoot = t.TempDir()
	checkout = filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(checkout, "cmd", "gt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "cmd", "gt", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot, checkout
}

// tailBareRepoDir makes the rig's bare repo as a plain directory under townRoot
// and returns its path. The fake git runners below answer for it; nothing here
// runs git.
func tailBareRepoDir(t *testing.T, townRoot string) string {
	t.Helper()
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	return bare
}

// tailStatFails is a filesystem look that fails every path with err, so a test
// can drive a stat failure without a real unreadable directory.
func tailStatFails(err error) func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return nil, err }
}

// TestTailDeployRepo_UsesTheBareRepoWhenItIsThere: the rig's bare repo answers
// for the deploy tracker and the main picture, not the checkout beside it.
func TestTailDeployRepo_UsesTheBareRepoWhenItIsThere(t *testing.T) {
	t.Parallel()
	townRoot, _ := tailDeployTown(t)
	bare := tailBareRepoDir(t, townRoot)

	repo, fellBack, err := tailDeployRepo(townRoot, os.Stat)
	if err != nil {
		t.Fatalf("tailDeployRepo: %v", err)
	}
	if repo != bare {
		t.Errorf("repo = %q, want the bare repo %q", repo, bare)
	}
	if fellBack != "" {
		t.Errorf("fellBack = %q, want none when the bare repo answers", fellBack)
	}
}

// TestTailDeployRepo_FallsBackToTheCheckoutWithoutABareRepo: a town with no
// .repo.git reads the source checkout, and the reason names both the absent
// bare repo and the checkout that answered.
func TestTailDeployRepo_FallsBackToTheCheckoutWithoutABareRepo(t *testing.T) {
	t.Parallel()
	townRoot, checkout := tailDeployTown(t)

	repo, fellBack, err := tailDeployRepo(townRoot, os.Stat)
	if err != nil {
		t.Fatalf("tailDeployRepo: %v", err)
	}
	if repo != checkout {
		t.Errorf("repo = %q, want the checkout %q", repo, checkout)
	}
	for _, want := range []string{"gastown/.repo.git is absent", checkout} {
		if !strings.Contains(fellBack, want) {
			t.Errorf("fellBack = %q, want it to name %q", fellBack, want)
		}
	}
}

// TestTailDeployRepo_RefusesWhatItCannotRead: a bare path that is not a
// directory, a stat that fails for any reason but absence, and a town with
// neither a bare repo nor a checkout are errors. Neither is a fallback, so
// neither answers from the wrong repository.
func TestTailDeployRepo_RefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) (townRoot string, stat func(string) (fs.FileInfo, error))
	}{
		{
			name: "the bare path is a file",
			setup: func(t *testing.T) (string, func(string) (fs.FileInfo, error)) {
				townRoot, _ := tailDeployTown(t)
				if err := os.WriteFile(filepath.Join(townRoot, "gastown", ".repo.git"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return townRoot, os.Stat
			},
		},
		{
			// The checkout is readable, so a fallback on any stat error would
			// answer from it; only the error keeps the stale checkout out.
			name: "the bare path is a symlink loop",
			setup: func(t *testing.T) (string, func(string) (fs.FileInfo, error)) {
				townRoot, _ := tailDeployTown(t)
				bare := filepath.Join(townRoot, "gastown", ".repo.git")
				if err := os.Symlink(bare, bare); err != nil {
					t.Fatal(err)
				}
				return townRoot, os.Stat
			},
		},
		{
			// A permission failure is not absence. The checkout is readable here
			// too, so only the error keeps it from answering.
			name: "the bare path cannot be read for permission",
			setup: func(t *testing.T) (string, func(string) (fs.FileInfo, error)) {
				townRoot, _ := tailDeployTown(t)
				return townRoot, tailStatFails(fs.ErrPermission)
			},
		},
		{
			name: "no bare repo and no checkout",
			setup: func(t *testing.T) (string, func(string) (fs.FileInfo, error)) {
				return t.TempDir(), os.Stat
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, stat := tc.setup(t)
			repo, fellBack, err := tailDeployRepo(townRoot, stat)
			if err == nil {
				t.Fatalf("tailDeployRepo = (%q, %q, nil), want an error", repo, fellBack)
			}
			if repo != "" {
				t.Errorf("repo = %q alongside the error, want none", repo)
			}
		})
	}
}

// TestTailGitAncestry_AsksTheBareRepo: every ancestry question runs git in the
// bare repo, with the commits as given, and git's exit code is the answer. A
// git that does not start is unknown, as is a git that exits with an error code.
func TestTailGitAncestry_AsksTheBareRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		code      int
		err       error
		want      bool
		wantKnown bool
	}{
		{name: "exit 0 is an ancestor", code: 0, want: true, wantKnown: true},
		{name: "exit 1 is not an ancestor", code: 1, want: false, wantKnown: true},
		{name: "a bad object is unknown", code: 128, want: false, wantKnown: false},
		{name: "a git that is killed on timeout is unknown", code: -1, want: false, wantKnown: false},
		{name: "a git that does not start is unknown", code: 0, err: errors.New("exec: git not found"), want: false, wantKnown: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, _ := tailDeployTown(t)
			bare := tailBareRepoDir(t, townRoot)
			var dirs []string
			var args []string
			run := func(dir string, a ...string) (int, error) {
				dirs = append(dirs, dir)
				args = a
				return tc.code, tc.err
			}

			ancestor := tailGitAncestryIn(townRoot, nil, os.Stat, run)
			ok, known := ancestor("c1", "c2")
			if ok != tc.want || known != tc.wantKnown {
				t.Errorf("ancestor(c1, c2) = (%v, %v), want (%v, %v)", ok, known, tc.want, tc.wantKnown)
			}
			if len(dirs) != 1 || dirs[0] != bare {
				t.Errorf("git ran in %q, want once in the bare repo %q", dirs, bare)
			}
			if want := "merge-base --is-ancestor c1 c2"; strings.Join(args, " ") != want {
				t.Errorf("git args = %q, want %q", strings.Join(args, " "), want)
			}
		})
	}
}

// TestTailGitAncestry_FallsBackToTheCheckoutAndSaysSo: a town with no bare repo
// asks the checkout, and the why-log names the checkout that answered, once.
func TestTailGitAncestry_FallsBackToTheCheckoutAndSaysSo(t *testing.T) {
	t.Parallel()
	townRoot, checkout := tailDeployTown(t)
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	var dirs []string
	run := func(dir string, _ ...string) (int, error) {
		dirs = append(dirs, dir)
		return 0, nil
	}
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	ancestor := tailGitAncestryIn(townRoot, why, os.Stat, run)
	if ok, known := ancestor("c1", "c2"); !ok || !known {
		t.Errorf("ancestor(c1, c2) = (%v, %v), want the checkout's answer (true, true)", ok, known)
	}
	if len(dirs) != 1 || dirs[0] != checkout {
		t.Errorf("git ran in %q, want once in the checkout %q", dirs, checkout)
	}
	want := "deploy repo: " + bare + " is absent; reading the source checkout " + checkout
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("why-log = %q, want exactly %q", lines, want)
	}
}

// TestTailGitAncestry_LogsAnUnreadableDeployRepo: a bare repo that cannot be
// read answers unknown without running git, and the why-log says so.
func TestTailGitAncestry_LogsAnUnreadableDeployRepo(t *testing.T) {
	t.Parallel()
	townRoot, _ := tailDeployTown(t)
	ran := false
	run := func(string, ...string) (int, error) {
		ran = true
		return 0, nil
	}
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	// Two ancestry builds are two polls of the same unreadable repo: the failure
	// is logged once, not on each.
	for range 2 {
		ancestor := tailGitAncestryIn(townRoot, why, tailStatFails(fs.ErrPermission), run)
		if ok, known := ancestor("c1", "c2"); ok || known {
			t.Errorf("ancestor(c1, c2) = (%v, %v), want unknown (false, false)", ok, known)
		}
	}
	if ran {
		t.Error("git ran with no deploy repo to ask, want no run")
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "deploy repo read failed: ") {
		t.Errorf("why-log = %q, want one line naming the failed read", lines)
	}
}

// TestTailMainPicture_ReadsTheBareRepo: the main picture asks the staleness
// check about the bare repo, and reports its main as the tip, the binary's
// commit as installed, and the commits between them as behind.
func TestTailMainPicture_ReadsTheBareRepo(t *testing.T) {
	t.Parallel()
	townRoot, _ := tailDeployTown(t)
	bare := tailBareRepoDir(t, townRoot)
	binary := strings.Repeat("a", 40)
	tip := strings.Repeat("b", 40)
	var repos []string
	check := func(repo, commit string) *version.StaleBinaryInfo {
		repos = append(repos, repo)
		if commit != binary {
			t.Errorf("check binary = %s, want the binary's commit %s", commit, binary)
		}
		return &version.StaleBinaryInfo{BinaryCommit: binary, RepoCommit: tip, CommitsBehind: 2}
	}

	gotTip, installed, behind, err := tailMainPictureIn(townRoot, binary, nil, os.Stat, check)
	if err != nil {
		t.Fatalf("tailMainPictureIn: %v", err)
	}
	if len(repos) != 1 || repos[0] != bare {
		t.Errorf("checked %q, want once the bare repo %q", repos, bare)
	}
	if want := version.ShortCommit(tip); gotTip != want {
		t.Errorf("main tip = %s, want %s", gotTip, want)
	}
	if want := version.ShortCommit(binary); installed != want {
		t.Errorf("installed = %s, want %s", installed, want)
	}
	if behind != 2 {
		t.Errorf("behind = %d, want 2", behind)
	}
}

// TestTailMainPicture_FallsBackToTheCheckoutAndSaysSo: a town with no bare repo
// checks the checkout, and the why-log names it.
func TestTailMainPicture_FallsBackToTheCheckoutAndSaysSo(t *testing.T) {
	t.Parallel()
	townRoot, checkout := tailDeployTown(t)
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	var repos []string
	check := func(repo, _ string) *version.StaleBinaryInfo {
		repos = append(repos, repo)
		return &version.StaleBinaryInfo{BinaryCommit: strings.Repeat("a", 40), RepoCommit: strings.Repeat("a", 40)}
	}
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	if _, _, _, err := tailMainPictureIn(townRoot, strings.Repeat("a", 40), why, os.Stat, check); err != nil {
		t.Fatalf("tailMainPictureIn: %v", err)
	}
	if len(repos) != 1 || repos[0] != checkout {
		t.Errorf("checked %q, want once the checkout %q", repos, checkout)
	}
	want := "deploy repo: " + bare + " is absent; reading the source checkout " + checkout
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("why-log = %q, want exactly %q", lines, want)
	}
}

// TestTailMainPicture_RefusesAnUnreadableDeployRepo: a bare repo that cannot be
// read is an error, and the staleness check never runs against a guess.
func TestTailMainPicture_RefusesAnUnreadableDeployRepo(t *testing.T) {
	t.Parallel()
	townRoot, _ := tailDeployTown(t)
	checked := 0
	check := func(string, string) *version.StaleBinaryInfo {
		checked++
		return nil
	}

	if _, _, _, err := tailMainPictureIn(townRoot, strings.Repeat("a", 40), nil, tailStatFails(fs.ErrPermission), check); err == nil {
		t.Error("tailMainPictureIn = nil error, want the unreadable bare repo refused")
	}
	if checked != 0 {
		t.Errorf("staleness check ran %d times, want none", checked)
	}
}

// TestTailDeployRepo_WhyLogLogsOnlyAChangeOfRepo: the fallback is logged once
// and not on every poll, and its return to the bare repo is logged once too.
// A nil log records nothing.
func TestTailDeployRepo_WhyLogLogsOnlyAChangeOfRepo(t *testing.T) {
	t.Parallel()
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	why.deployRepo("absent; reading the checkout")
	why.deployRepo("absent; reading the checkout")
	why.deployRepo("")
	why.deployRepo("")

	if len(lines) != 2 {
		t.Fatalf("logged %d lines %q, want the fallback and the return once each", len(lines), lines)
	}
	if want := "deploy repo: absent; reading the checkout"; lines[0] != want {
		t.Errorf("fallback line = %q, want %q", lines[0], want)
	}
	if lines[1] != "deploy repo is the rig's bare repo again" {
		t.Errorf("return line = %q, want the bare repo's return", lines[1])
	}
	var nilLog *dashWhyLog
	nilLog.deployRepo("absent") // must not panic
}

// TestTailDeployRepo_UnreadableBareRepoLogsOnceThroughWhyLog: gt tail's why-log
// writes an unreadable bare repo's reason once to its stderr, however many
// ancestry checks are built on it, and every ancestry call answers unknown. The
// fake look fails the bare path with a permission error, so no git runs.
func TestTailDeployRepo_UnreadableBareRepoLogsOnceThroughWhyLog(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	run := func(string, ...string) (int, error) {
		t.Error("git ran for an unreadable bare repo")
		return 0, nil
	}
	var stderr strings.Builder
	why := tailWhyLog(&stderr)
	for i := 0; i < 2; i++ {
		ancestor := tailGitAncestryIn(townRoot, why, tailStatFails(fs.ErrPermission), run)
		if ok, known := ancestor("c1", "c2"); ok || known {
			t.Errorf("ancestor(c1, c2) = (%v, %v), want unknown (false, false)", ok, known)
		}
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "gt tail: deploy repo read failed: ") {
		t.Errorf("why-log = %q, want exactly one gt tail line naming the unreadable bare repo", stderr.String())
	}
}

// TestTailGitAncestry_AGitThatDidNotStartIsAnError: a git run that never started
// is an error, not an exit code, so the ancestry check reads it as unknown. The
// integration tier runs the real start failure; this drives the classification.
func TestTailGitAncestry_AGitThatDidNotStartIsAnError(t *testing.T) {
	t.Parallel()
	start := errors.New("chdir: no such file or directory")
	code, err := tailGitExit(start)
	if !errors.Is(err, start) {
		t.Errorf("tailGitExit(start failure) error = %v, want the start failure returned", err)
	}
	if code != 0 {
		t.Errorf("tailGitExit(start failure) code = %d, want 0 with the error", code)
	}
}
