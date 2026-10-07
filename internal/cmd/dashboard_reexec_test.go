package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// watchHarness drives watchFiles one tick at a time. ticks is unbuffered, so a
// send returns only after the watcher has finished the previous tick.
type watchHarness struct {
	stamps []chan stampResult // one per watched file, in watch order
	ticks  chan time.Time
	done   chan struct{}
	cancel context.CancelFunc
	fired  int
	reason string
}

type stampResult struct {
	s   fileStamp
	err error
}

// startWatch watches one file, the binary, whose first reading is first.
func startWatch(t *testing.T, first stampResult) *watchHarness {
	t.Helper()
	return startWatches(t, []string{"binary"}, []stampResult{first})
}

// startWatches watches the named files, with the given baseline readings. Every
// tick reads the files in this order, so a test names one reading per file.
func startWatches(t *testing.T, names []string, firsts []stampResult) *watchHarness {
	t.Helper()
	h := &watchHarness{ticks: make(chan time.Time), done: make(chan struct{})}
	files := make([]watchedFile, len(names))
	for i, name := range names {
		ch := make(chan stampResult, 16)
		ch <- firsts[i]
		h.stamps = append(h.stamps, ch)
		files[i] = watchedFile{name: name, stamp: func() (fileStamp, error) { r := <-ch; return r.s, r.err }}
	}
	var ctx context.Context
	ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	go func() {
		defer close(h.done)
		watchFiles(ctx, files, h.ticks, func(name string) { h.fired++; h.reason = name })
	}()
	return h
}

// tick queues one stamp reading per watched file, in watch order, and delivers
// one tick.
func (h *watchHarness) tick(rs ...stampResult) {
	for i, r := range rs {
		h.stamps[i] <- r
	}
	h.ticks <- time.Time{}
}

// finish stops the watcher and reports how many times it fired. reason holds the
// name it fired with.
func (h *watchHarness) finish() int {
	h.cancel()
	<-h.done
	return h.fired
}

func TestWatchBinaryFiresOnceAfterReplacement(t *testing.T) {
	t.Parallel()
	v1, v2 := fileStamp{size: 1, ino: 1}, fileStamp{size: 2, ino: 2}
	h := startWatch(t, stampResult{s: v1})
	h.tick(stampResult{s: v1})
	h.tick(stampResult{s: v2}) // first sight of the new build: not yet
	h.tick(stampResult{s: v2}) // held for a second tick: fire
	<-h.done
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times, want 1", got)
	}
}

func TestWatchBinaryWaitsOutAFileStillBeingWritten(t *testing.T) {
	t.Parallel()
	v1, growing, v2 := fileStamp{size: 1}, fileStamp{size: 5}, fileStamp{size: 9}
	h := startWatch(t, stampResult{s: v1})
	h.tick(stampResult{s: growing})
	h.tick(stampResult{s: v2}) // size moved again: restart the count
	h.tick(stampResult{s: v1}) // back to the original: forget it
	h.tick(stampResult{s: v1})
	if got := h.finish(); got != 0 {
		t.Fatalf("fired %d times on an unsettled file, want 0", got)
	}
}

func TestWatchBinaryIgnoresMissingFile(t *testing.T) {
	t.Parallel()
	v1, v2 := fileStamp{size: 1}, fileStamp{size: 2}
	h := startWatch(t, stampResult{s: v1})
	h.tick(stampResult{s: v2})
	h.tick(stampResult{err: errors.New("no such file")}) // rename in flight: forget the sighting
	h.tick(stampResult{s: v2})
	if got := h.finish(); got != 0 {
		t.Fatalf("a missing file counted toward a change: fired %d times", got)
	}
}

// A baseline that cannot be read at the start must not switch the watcher off:
// it is read on a later tick and a replacement after that is still seen.
func TestWatchBinaryRecoversFromAFailedBaseline(t *testing.T) {
	t.Parallel()
	v1, v2 := fileStamp{size: 1}, fileStamp{size: 2}
	h := startWatch(t, stampResult{err: errors.New("no such file")})
	h.tick(stampResult{err: errors.New("still missing")})
	h.tick(stampResult{s: v1}) // baseline at last
	h.tick(stampResult{s: v2})
	h.tick(stampResult{s: v2})
	<-h.done
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times after a late baseline, want 1", got)
	}
}

// A rig added to the town rewrites the registry file: the dashboard restarts
// once the new stamp has held for two ticks, and says which file it followed.
func TestWatchFilesFiresOnceOnASettledRegistryChange(t *testing.T) {
	t.Parallel()
	bin := fileStamp{size: 1, ino: 1}
	reg1, reg2 := fileStamp{size: 10, ino: 2}, fileStamp{size: 11, ino: 3}
	h := startWatches(t, []string{"binary", "town registry"}, []stampResult{{s: bin}, {s: reg1}})
	h.tick(stampResult{s: bin}, stampResult{s: bin})
	h.tick(stampResult{s: bin}, stampResult{s: reg2}) // first sight of the new registry: not yet
	h.tick(stampResult{s: bin}, stampResult{s: reg2}) // held for a second tick: fire
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times, want 1", got)
	}
	if h.reason != "town registry" {
		t.Fatalf("restarted for %q, want the town registry", h.reason)
	}
}

func TestWatchFilesIgnoresAnUnchangedRegistry(t *testing.T) {
	t.Parallel()
	bin, reg := fileStamp{size: 1, ino: 1}, fileStamp{size: 10, ino: 2}
	h := startWatches(t, []string{"binary", "town registry"}, []stampResult{{s: bin}, {s: reg}})
	h.tick(stampResult{s: bin}, stampResult{s: reg})
	h.tick(stampResult{s: bin}, stampResult{s: reg})
	if got := h.finish(); got != 0 {
		t.Fatalf("an unchanged registry fired %d times, want 0", got)
	}
}

func TestWatchFilesWaitsOutARegistryStillBeingWritten(t *testing.T) {
	t.Parallel()
	bin := fileStamp{size: 1, ino: 1}
	reg1, half, reg2 := fileStamp{size: 10}, fileStamp{size: 14}, fileStamp{size: 22}
	h := startWatches(t, []string{"binary", "town registry"}, []stampResult{{s: bin}, {s: reg1}})
	h.tick(stampResult{s: bin}, stampResult{s: half})
	h.tick(stampResult{s: bin}, stampResult{s: reg2}) // written on again: restart the count
	h.tick(stampResult{s: bin}, stampResult{s: reg1}) // back to the original: forget it
	h.tick(stampResult{s: bin}, stampResult{s: reg1})
	if got := h.finish(); got != 0 {
		t.Fatalf("an unsettled registry fired %d times, want 0", got)
	}
}

// The registry is missing when the dashboard starts on a machine that has no
// registry yet; the watch must keep looking, and a baseline that arrives later
// must not read as a change of its own.
func TestWatchFilesRetriesAMissingRegistry(t *testing.T) {
	t.Parallel()
	bin := fileStamp{size: 1, ino: 1}
	reg1, reg2 := fileStamp{size: 10}, fileStamp{size: 11}
	h := startWatches(t, []string{"binary", "town registry"}, []stampResult{{s: bin}, {err: errors.New("no such file")}})
	h.tick(stampResult{s: bin}, stampResult{err: errors.New("still missing")})
	h.tick(stampResult{s: bin}, stampResult{s: reg1}) // baseline at last
	h.tick(stampResult{s: bin}, stampResult{s: reg2})
	h.tick(stampResult{s: bin}, stampResult{s: reg2})
	<-h.done
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times after a late registry baseline, want 1", got)
	}
	if h.reason != "town registry" {
		t.Fatalf("restarted for %q, want the town registry", h.reason)
	}
}

func TestWatchFilesIgnoresARegistryStampErrorAfterTheBaseline(t *testing.T) {
	t.Parallel()
	bin := fileStamp{size: 1, ino: 1}
	reg1, reg2 := fileStamp{size: 10}, fileStamp{size: 11}
	h := startWatches(t, []string{"binary", "town registry"}, []stampResult{{s: bin}, {s: reg1}})
	h.tick(stampResult{s: bin}, stampResult{s: reg2})
	h.tick(stampResult{s: bin}, stampResult{err: errors.New("rename in flight")})
	h.tick(stampResult{s: bin}, stampResult{s: reg2})
	if got := h.finish(); got != 0 {
		t.Fatalf("a registry that vanished mid-write fired %d times, want 0", got)
	}
}

// The two watches are independent: the registry sitting still does not stop the
// binary from being followed.
func TestWatchFilesNamesTheFileThatChanged(t *testing.T) {
	t.Parallel()
	bin1, bin2 := fileStamp{size: 1, ino: 1}, fileStamp{size: 2, ino: 2}
	reg := fileStamp{size: 10, ino: 3}
	h := startWatches(t, []string{"binary", "town registry"}, []stampResult{{s: bin1}, {s: reg}})
	h.tick(stampResult{s: bin2}, stampResult{s: reg})
	h.tick(stampResult{s: bin2}, stampResult{s: reg})
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times, want 1", got)
	}
	if h.reason != "binary" {
		t.Fatalf("restarted for %q, want the binary", h.reason)
	}
}

// stampFile is the real reader the dashboard runs on: the same file reads the
// same, and a file renamed over it, which is how install replaces the binary,
// reads differently.
func TestStampFileTellsAReplacedFileApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "gt")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := stampFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := stampFile(path); again != a {
		t.Fatalf("unchanged file stamped differently: %+v vs %+v", a, again)
	}
	next := filepath.Join(dir, "gt.new")
	if err := os.WriteFile(next, []byte("a longer second build"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	b, err := stampFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b == a {
		t.Fatalf("a replaced file kept its stamp: %+v", a)
	}
	if _, err := stampFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file stamped without error")
	}
}

// The live watch list reads the executable and the town registry off disk, so a
// restart follows the files the shell and gt rig add actually write.
func TestRestartWatchesFollowTheExecutableAndTheRegistry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "gt")
	registry := dashboardRegistryPath(dir)
	if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{exe, registry} {
		if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	watches := restartWatches(exe, registry)
	if len(watches) != 2 || watches[0].name != "binary" || watches[1].name != "town registry" {
		t.Fatalf("restartWatches = %+v, want the binary then the town registry", watches)
	}
	for i, path := range []string{exe, registry} {
		before, err := watches[i].stamp()
		if err != nil {
			t.Fatalf("%s: %v", watches[i].name, err)
		}
		if want, _ := stampFile(path); before != want {
			t.Fatalf("%s stamped %+v, want %+v", watches[i].name, before, want)
		}
		if err := os.WriteFile(path, []byte("rewritten, and longer"), 0o644); err != nil {
			t.Fatal(err)
		}
		after, err := watches[i].stamp()
		if err != nil {
			t.Fatalf("%s: %v", watches[i].name, err)
		}
		if after == before {
			t.Fatalf("the rewritten %s kept its stamp: %+v", watches[i].name, before)
		}
	}
}

// The registry a restart follows is the file gt rig add rewrites: the machine
// config file (config layout.go), under the town root.
func TestDashboardRegistryPathIsTheMachineConfigFile(t *testing.T) {
	t.Parallel()
	if got, want := dashboardRegistryPath("/town"), filepath.Join("/town", "mayor", "town.json"); got != want {
		t.Fatalf("dashboardRegistryPath = %q, want %q", got, want)
	}
}

func TestHandoffKeepsServingWhenTheNewBinaryDoesNotRun(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	execd := false
	err := handoff(
		"binary",
		func() error { return errors.New("signal: killed") },
		func() error { execd = true; return nil },
		&out,
	)
	if err == nil || !strings.Contains(err.Error(), "the gt binary does not run") {
		t.Fatalf("err = %v, want the gt binary does not run", err)
	}
	if execd {
		t.Fatal("exec'd over a binary that failed verification")
	}
	if strings.Contains(out.String(), "restarting") {
		t.Fatalf("announced a restart that did not happen: %q", out.String())
	}
}

func TestHandoffReportsAFailedExec(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := handoff("binary", func() error { return nil }, func() error { return errors.New("exec format error") }, &out)
	if err == nil || !strings.Contains(err.Error(), "re-exec") {
		t.Fatalf("err = %v, want a re-exec failure", err)
	}
	if want := "gt dashboard: binary changed, restarting"; !strings.Contains(out.String(), want) {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
}

// A registry change restarts the running binary in place; the line names the
// registry so an operator reading the log can tell it from a binary install.
func TestHandoffNamesTheRegistryChange(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	verified := false
	err := handoff(
		"town registry",
		func() error { verified = true; return nil },
		func() error { return errors.New("exec format error") },
		&out,
	)
	if !verified {
		t.Fatal("restarted on a registry change without checking the binary still runs")
	}
	if err == nil {
		t.Fatal("want the re-exec failure")
	}
	if want := "gt dashboard: town registry changed, restarting"; !strings.Contains(out.String(), want) {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
}

func TestExecEnvReplacesAStaleListenerFD(t *testing.T) {
	t.Parallel()
	in := []string{"A=1", dashboardListenFDEnv + "=7", "B=2"}
	got := execEnv(in, 9)
	want := []string{"A=1", "B=2", dashboardListenFDEnv + "=9"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("execEnv = %v, want %v", got, want)
	}
	if len(in) != 3 || in[1] != dashboardListenFDEnv+"=7" {
		t.Fatalf("execEnv modified its input: %v", in)
	}
}
