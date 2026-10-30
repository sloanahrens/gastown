package uninstallgt

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunMissingDirectoryExitsZero pins the run's first case: a directory that
// is not there has nothing to remove, so the run says so and succeeds.
func TestMissingDirectoryExitsZero(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	rec := newRecorder()

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil))

	if code != ExitOK {
		t.Fatalf("Run on a missing directory = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	want := Program + ": nothing to do — " + h.bin + " does not exist\n"
	if !strings.HasSuffix(stdout.String(), want) {
		t.Errorf("Run on a missing directory printed %q, want it to end with %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), "not present:") {
		t.Errorf("Run on a missing directory reported its files as not present: %q", stdout.String())
	}
	if got := rec.removed(); len(got) != 0 {
		t.Errorf("Run on a missing directory removed %v, want nothing", got)
	}
}

// TestRunAbsentFilesAreReportedNotErrors pins that an install directory with
// neither binary is a report and a zero exit, not a failure.
func TestAbsentFilesAreReportedNotErrors(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	rec := newRecorder()
	rec.add(h.bin, entry{dir: true})

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil))

	if code != ExitOK {
		t.Fatalf("Run on a directory with neither binary = %d, want %d (stderr: %s)",
			code, ExitOK, stderr.String())
	}
	for _, name := range []string{"gt", "bd"} {
		want := Program + ": not present: " + filepath.Join(h.bin, name) + "\n"
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("Run printed %q, want it to hold %q", stdout.String(), want)
		}
	}
	if want := "done — 0 removed from " + h.bin; !strings.Contains(stdout.String(), want) {
		t.Errorf("Run printed %q, want it to hold %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("Run on a directory with neither binary wrote %q to stderr, want nothing", stderr.String())
	}
}

// TestRunFailureExitsOneAndSaysNothingElseWasTouched pins the failure contract:
// the run keeps going after a path it could not remove, ends exit 1, and says
// on stderr that the removal was all it touched.
func TestFailureExitsOneAndSaysNothingElseWasTouched(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	gt, bd := filepath.Join(h.bin, "gt"), filepath.Join(h.bin, "bd")
	rec := newRecorder()
	rec.add(h.bin, entry{dir: true})
	rec.add(gt, entry{survives: true})
	rec.add(bd, entry{})

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil))

	if code != ExitFailed {
		t.Fatalf("Run with a path it could not remove = %d, want %d", code, ExitFailed)
	}
	if want := "FAILED to remove " + gt + " (still present after rm -f)\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("Run wrote %q to stderr, want it to hold %q", stderr.String(), want)
	}
	if want := "1 removed, at least one failed; nothing else was touched\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("Run wrote %q to stderr, want it to hold %q", stderr.String(), want)
	}
	if !strings.Contains(stdout.String(), "removed "+bd+"\n") {
		t.Errorf("Run printed %q, want the other binary's removal", stdout.String())
	}
	if !rec.has(gt) {
		t.Errorf("%s was removed although its removal failed", gt)
	}
}

// TestRunRefusesTheLiveInstall is the incident (gt-acdfp): the run stops dead
// before touching a binary a town resolves off PATH.
func TestRunRefusesTheLiveInstall(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	rec := newRecorder()
	rec.add(h.live, entry{dir: true})
	rec.add(filepath.Join(h.live, "gt"), entry{})
	rec.add(filepath.Join(h.live, "bd"), entry{})
	env := Env{
		Getenv:  vars(map[string]string{"HOME": h.home, "GT_TOWN_ROOT": h.town}),
		Files:   rec,
		Exec:    rec.exec,
		Signal0: neverSignals,
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.live}, &stdout, &stderr, env)

	if code != ExitRefused {
		t.Fatalf("Run on the live install = %d, want %d (stderr: %s)", code, ExitRefused, stderr.String())
	}
	want := Program + ": refusing to run: " + h.live + " is the live install of the town at " + h.town
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("Run wrote %q to stderr, want it to hold %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("Run printed %q to stdout beside the refusal, want nothing", stdout.String())
	}
	if got := rec.removed(); len(got) != 0 {
		t.Errorf("Run on the live install removed %v, want nothing", got)
	}
}

// TestRunRefusalRemovesNothing pins the check the whole command exists for
// (gt-acdfp): a refused run makes no filesystem call at all, so it cannot have
// removed anything before the check answered.
func TestRunRefusalRemovesNothing(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	rec := newRecorder()
	rec.add(h.live, entry{dir: true})
	rec.add(filepath.Join(h.live, "gt"), entry{})
	rec.add(filepath.Join(h.live, "bd"), entry{})
	env := Env{
		Getenv:  vars(map[string]string{"HOME": h.home, "GT_TOWN_ROOT": h.town}),
		Files:   rec,
		Exec:    rec.exec,
		Signal0: neverSignals,
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.live}, &stdout, &stderr, env)

	if code != ExitRefused {
		t.Fatalf("Run on the live install = %d, want %d (stderr: %s)", code, ExitRefused, stderr.String())
	}
	if len(rec.ops) != 0 {
		t.Errorf("a refused run made these filesystem or command calls: %v, want none", rec.ops)
	}
}

// TestRunChecksLiveBeforeRemoving pins the order inside the run: the live
// check's announcement comes before the target line and before any removal, so
// a forced run cannot start removing first and decide afterwards.
func TestRunChecksLiveBeforeRemoving(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	gt, bd := filepath.Join(h.live, "gt"), filepath.Join(h.live, "bd")
	rec := newRecorder()
	rec.add(h.live, entry{dir: true})
	rec.add(gt, entry{})
	rec.add(bd, entry{})
	env := Env{
		Getenv:  vars(map[string]string{"HOME": h.home, "GT_TOWN_ROOT": h.town}),
		Files:   rec,
		Exec:    rec.exec,
		Signal0: neverSignals,
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.live, "--force"}, &stdout, &stderr, env)

	if code != ExitOK {
		t.Fatalf("Run --force on the live install = %d, want %d (stderr: %s)",
			code, ExitOK, stderr.String())
	}
	notice := Program + ": FORCE=1 — removing the live install of the town at " + h.town
	target := Program + ": target " + h.live
	removed := []string{Program + ": removed " + gt, Program + ": removed " + bd}
	lines := strings.SplitAfter(stdout.String(), "\n")
	noticeAt, targetAt := indexOfLine(lines, notice), indexOfLine(lines, target)
	if noticeAt < 0 || targetAt < 0 {
		t.Fatalf("Run printed %q, want both the FORCE announcement and the target line", stdout.String())
	}
	if noticeAt > targetAt {
		t.Errorf("the live-check announcement came at line %d, after the target line at %d", noticeAt, targetAt)
	}
	for _, line := range removed {
		at := indexOfLine(lines, line)
		if at < 0 {
			t.Fatalf("Run printed %q, want it to hold %q", stdout.String(), line)
		}
		if noticeAt > at {
			t.Errorf("the live-check announcement came at line %d, after the removal at %d", noticeAt, at)
		}
	}
}

// indexOfLine returns the index of the first line starting with prefix, or -1.
func indexOfLine(lines []string, prefix string) int {
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return i
		}
	}
	return -1
}

// TestRunForceRemovesTheLiveInstall pins --force: the run announces what it is
// about to take down and then does it.
func TestRunForceRemovesTheLiveInstall(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	gt, bd := filepath.Join(h.live, "gt"), filepath.Join(h.live, "bd")
	rec := newRecorder()
	rec.add(h.live, entry{dir: true})
	rec.add(gt, entry{})
	rec.add(bd, entry{})
	env := Env{
		Getenv:  vars(map[string]string{"HOME": h.home, "GT_TOWN_ROOT": h.town}),
		Files:   rec,
		Exec:    rec.exec,
		Signal0: neverSignals,
	}

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.live, "--force"}, &stdout, &stderr, env)

	if code != ExitOK {
		t.Fatalf("Run --force on the live install = %d, want %d (stderr: %s)",
			code, ExitOK, stderr.String())
	}
	if want := Program + ": FORCE=1 — removing the live install of the town at " + h.town; !strings.Contains(stdout.String(), want) {
		t.Errorf("Run printed %q, want it to hold %q", stdout.String(), want)
	}
	if got, want := rec.removed(), []string{gt, bd}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Run --force removed %v, want %v", got, want)
	}
}

// TestRunCannotExamineTheDirectory pins the fail-closed branch: a directory
// whose stat fails for a reason other than absence is not a directory with
// nothing in it.
func TestRunCannotExamineTheDirectory(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	rec := newRecorder()
	rec.add(h.bin, entry{statErr: fs.ErrPermission})

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil))

	if code != ExitFailed {
		t.Fatalf("Run on an unreadable directory = %d, want %d", code, ExitFailed)
	}
	if want := "cannot examine " + h.bin + "; nothing was removed\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("Run wrote %q to stderr, want it to hold %q", stderr.String(), want)
	}
	if got := rec.removed(); len(got) != 0 {
		t.Errorf("Run on an unreadable directory removed %v, want nothing", got)
	}
}

// TestRunTreatsANonDirectoryAsNothingToDo pins a target that stopped being a
// directory after Resolve checked it: there is nothing to remove from it.
func TestRunTreatsANonDirectoryAsNothingToDo(t *testing.T) {
	t.Parallel()
	h := newRunHost(t)
	rec := newRecorder()
	rec.add(h.bin, entry{})

	var stdout, stderr strings.Builder
	code := Run([]string{"--install-dir", h.bin}, &stdout, &stderr, h.env(rec, nil))

	if code != ExitOK {
		t.Fatalf("Run on a non-directory = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	if want := "nothing to do — " + h.bin + " is not a directory\n"; !strings.HasSuffix(stdout.String(), want) {
		t.Errorf("Run printed %q, want it to end with %q", stdout.String(), want)
	}
	if got := rec.removed(); len(got) != 0 {
		t.Errorf("Run on a non-directory removed %v, want nothing", got)
	}
}
