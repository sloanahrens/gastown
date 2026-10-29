package baks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// exitErr is a process exit status as os/exec reports one, so a test can tell
// lsof's "nothing found" exit 1 from a real failure without running lsof.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

// fakeLsof answers canned output per lsof command line and records every call.
// A command no test answered fails the test, so a check the target should have
// skipped cannot pass unnoticed.
type fakeLsof struct {
	t      *testing.T
	reply  map[string]lsofReply
	calls  []string
	cancel context.CancelFunc // called before answering, to model a run cut short
}

type lsofReply struct {
	out []byte
	err error
}

func newFakeLsof(t *testing.T) *fakeLsof {
	t.Helper()
	return &fakeLsof{t: t, reply: map[string]lsofReply{}}
}

// open answers that the processes whose pids are pid hold path.
func (f *fakeLsof) open(path, pid string) { f.answer(path, pid+"\n", nil) }

// closed answers lsof's "nothing holds this file": exit 1 with no output.
func (f *fakeLsof) closed(path string) { f.answer(path, "", exitErr(1)) }

// failed answers an lsof that could not check path at all.
func (f *fakeLsof) failed(path string, code int) { f.answer(path, "", exitErr(code)) }

func (f *fakeLsof) answer(path, out string, err error) {
	f.reply["lsof -t -- "+path] = lsofReply{out: []byte(out), err: err}
}

func (f *fakeLsof) run(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	if f.cancel != nil {
		f.cancel()
	}
	r, ok := f.reply[line]
	if !ok {
		f.t.Errorf("unexpected command: %s", line)
		return nil, errors.New("no canned reply")
	}
	return r.out, r.err
}

// writeBackup writes a file of size bytes in dir, aged age before testNow, and
// returns its path. The mtime is set with Chtimes, so an age costs no wait.
func writeBackup(t *testing.T, dir, name string, age time.Duration, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	when := testNow.Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

// mustMkdir creates path and every parent.
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestDefaultDirs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	home := t.TempDir()
	mustMkdir(t, filepath.Join(town, "settings"))
	mustMkdir(t, filepath.Join(town, "gastown", "settings"))
	mustMkdir(t, filepath.Join(town, "beads", "settings"))
	// mayor holds no settings directory, and neither does a bare directory, so
	// neither is a rig.
	mustMkdir(t, filepath.Join(town, "mayor"))
	mustMkdir(t, filepath.Join(town, "loose"))

	config, binary := DefaultDirs(town, home)

	wantConfig := []string{
		filepath.Join(town, "settings"),
		filepath.Join(town, "beads", "settings"),
		filepath.Join(town, "gastown", "settings"),
		filepath.Join(town, "mayor"),
		filepath.Join(home, ".config", "om"),
	}
	if !reflect.DeepEqual(config, wantConfig) {
		t.Fatalf("config dirs = %v, want %v", config, wantConfig)
	}
	wantBinary := []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "go", "bin")}
	if !reflect.DeepEqual(binary, wantBinary) {
		t.Fatalf("binary dirs = %v, want %v", binary, wantBinary)
	}

	// A blank town or home is skipped rather than joined into a relative path,
	// which would list the working directory.
	if config, binary := DefaultDirs("", ""); len(config) != 0 || len(binary) != 0 {
		t.Errorf("DefaultDirs(\"\", \"\") = %v, %v; want nothing", config, binary)
	}
}

func TestTargetPlanKeepsOpenAndUnknownFiles(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	// Two backups per binary are kept by the keep count, so the third and
	// oldest is the one lsof's answer decides.
	gtNew := writeBackup(t, binDir, "gt.bak-01", 1*time.Hour, 10)
	gtMid := writeBackup(t, binDir, "gt.bak-02", 2*time.Hour, 10)
	gtOld := writeBackup(t, binDir, "gt.bak-03", 3*time.Hour, 10)
	omNew := writeBackup(t, binDir, "om.bak-01", 4*time.Hour, 10)
	omMid := writeBackup(t, binDir, "om.bak-02", 5*time.Hour, 10)
	omOld := writeBackup(t, binDir, "om.bak-03", 6*time.Hour, 10)
	notBackup := writeBackup(t, binDir, "NOTES.txt", 7*time.Hour, 10)

	lsof := newFakeLsof(t)
	lsof.open(gtOld, "4242")
	lsof.failed(omOld, 2)
	for _, path := range []string{gtNew, gtMid, omNew, omMid} {
		lsof.closed(path)
	}

	plan, err := NewTarget(Config{BinaryDirs: []string{binDir}, Run: lsof.run}).
		Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused != "" {
		t.Fatalf("Refused = %q, want a plan", plan.Refused)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("actions = %+v, want none: an open file and one lsof could not check are kept", plan.Actions)
	}
	if rule, ok := keptRule(plan, gtOld); !ok || rule != "open by 4242" {
		t.Errorf("oldest gt backup rule = %q (kept %v), want %q", rule, ok, "open by 4242")
	}
	if rule, ok := keptRule(plan, omOld); !ok || rule != "open by lsof unavailable" {
		t.Errorf("oldest om backup rule = %q (kept %v), want %q", rule, ok, "open by lsof unavailable")
	}
	if rule, ok := keptRule(plan, notBackup); !ok || rule != "not a backup" {
		t.Errorf("plain file rule = %q (kept %v), want %q", rule, ok, "not a backup")
	}
	if got := len(lsof.calls); got != 6 {
		t.Errorf("lsof calls = %d (%v), want 6: one per candidate and none for the plain file", got, lsof.calls)
	}
}

func TestTargetPlanSkipsSymlinksAndDirs(t *testing.T) {
	t.Parallel()
	cfgDir := t.TempDir()
	backup := writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)
	link := filepath.Join(cfgDir, "app.toml.bak-link")
	if err := os.Symlink(backup, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dir := filepath.Join(cfgDir, "app.toml.bak-dir")
	mustMkdir(t, dir)
	writeBackup(t, dir, "inside.toml.bak-01", 40*24*time.Hour, 10)

	lsof := newFakeLsof(t)
	lsof.closed(backup)

	plan, err := NewTarget(Config{ConfigDirs: []string{cfgDir}, Run: lsof.run}).
		Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("actions = %+v, want none", plan.Actions)
	}
	if _, ok := keptRule(plan, backup); !ok {
		t.Errorf("%s is not accounted for, want it in the plan", backup)
	}
	for _, path := range []string{link, dir} {
		if rule, ok := keptRule(plan, path); ok {
			t.Errorf("%s reached the planner as kept (%q), want a symlink and a directory skipped", path, rule)
		}
	}
	if got := len(lsof.calls); got != 1 {
		t.Errorf("lsof calls = %d (%v), want only the regular file", got, lsof.calls)
	}
}

func TestDefaultDirsHandsOverUnjudgedPaths(t *testing.T) {
	t.Parallel()
	// A settings name that links to itself cannot be checked: stat fails with a
	// loop error, which is neither "not there" nor "not a rig".
	loopRig := t.TempDir()
	mustMkdir(t, filepath.Join(loopRig, "gastown"))
	loopSettings := filepath.Join(loopRig, "beads", "settings")
	mustMkdir(t, filepath.Dir(loopSettings))
	if err := os.Symlink(loopSettings, loopSettings); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	loopTown := filepath.Join(t.TempDir(), "town")
	if err := os.Symlink(loopTown, loopTown); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name string
		town string
		want string // the path the plan must refuse on
	}{
		{name: "rig settings cannot be checked", town: loopRig, want: loopSettings},
		{name: "town cannot be listed", town: loopTown, want: loopTown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config, _ := DefaultDirs(tc.town, t.TempDir())
			lsof := newFakeLsof(t)
			plan, err := NewTarget(Config{ConfigDirs: config, Run: lsof.run}).
				Plan(context.Background(), diskmaint.Policy{Now: testNow})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.Refused == "" || !strings.Contains(plan.Refused, tc.want) {
				t.Fatalf("Refused = %q, want an unreadable-state refusal naming %s", plan.Refused, tc.want)
			}
		})
	}
}

func TestTargetPlanIgnoresAMissingDir(t *testing.T) {
	t.Parallel()
	// A configured directory that is not there holds no backups, so it is not
	// a read error and does not refuse the plan.
	absent := filepath.Join(t.TempDir(), "settings")

	lsof := newFakeLsof(t)
	target := NewTarget(Config{ConfigDirs: []string{absent}, Run: lsof.run})
	plan, err := target.Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused != "" {
		t.Fatalf("Refused = %q, want a plan for a directory that is not there", plan.Refused)
	}
	if len(plan.Actions) != 0 || len(plan.Kept) != 0 {
		t.Fatalf("plan = %+v, want nothing from a missing directory", plan)
	}
	if got := len(lsof.calls); got != 0 {
		t.Errorf("lsof calls = %d, want none with no candidate", got)
	}
}

func TestTargetPlanRefusesUnreadableDir(t *testing.T) {
	t.Parallel()
	good := t.TempDir()
	writeBackup(t, good, "app.toml.bak-01", 40*24*time.Hour, 10)

	// A regular file cannot be listed by anyone, root included, so this needs no
	// permissions to be unreadable.
	bad := filepath.Join(t.TempDir(), "settings")
	if err := os.WriteFile(bad, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	lsof := newFakeLsof(t)
	target := NewTarget(Config{ConfigDirs: []string{good, bad}, Run: lsof.run})
	plan, err := target.Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused == "" || !strings.Contains(plan.Refused, bad) {
		t.Fatalf("Refused = %q, want an unreadable-state refusal naming %s", plan.Refused, bad)
	}
	if len(plan.Actions) != 0 || len(plan.Kept) != 0 {
		t.Fatalf("plan = %+v, want empty when refused", plan)
	}
	if got := len(lsof.calls); got != 0 {
		t.Errorf("lsof calls = %d, want none over a plan that is already refused", got)
	}

	if _, err := target.Measure(context.Background()); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("Measure error = %v, want it to name %s", err, bad)
	}
}

func TestTargetPlanReadsTheInstalledBinary(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		installed bool
		wantGone  bool
		wantRule  string
	}{
		"installed bd newer than the build: the build goes": {installed: true, wantGone: true},
		"no installed bd to compare with: the build stays":  {wantRule: "installed binary unreadable"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			binDir := t.TempDir()
			if tc.installed {
				writeBackup(t, binDir, "bd", 1*time.Hour, 10)
			}
			build := writeBackup(t, binDir, "bd.abcdef1", 10*24*time.Hour, 10)

			lsof := newFakeLsof(t)
			lsof.closed(build)

			plan, err := NewTarget(Config{
				BinaryDirs: []string{binDir},
				Installed:  []string{"bd"},
				Run:        lsof.run,
			}).Plan(context.Background(), diskmaint.Policy{Now: testNow})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if tc.wantGone {
				if want := []string{build}; !reflect.DeepEqual(removed(plan), want) {
					t.Fatalf("removed = %v, want %v", removed(plan), want)
				}
				return
			}
			if len(plan.Actions) != 0 {
				t.Fatalf("actions = %+v, want none", plan.Actions)
			}
			if rule, ok := keptRule(plan, build); !ok || rule != tc.wantRule {
				t.Fatalf("build rule = %q (kept %v), want %q", rule, ok, tc.wantRule)
			}
		})
	}
}

func TestTargetMeasureCountsCandidates(t *testing.T) {
	t.Parallel()
	cfgDir := t.TempDir()
	binDir := t.TempDir()
	writeBackup(t, cfgDir, "app.toml.bak-01", 40*24*time.Hour, 10)
	writeBackup(t, cfgDir, "app.toml.bak-02", 41*24*time.Hour, 20)
	writeBackup(t, cfgDir, "app.toml", 42*24*time.Hour, 5) // the live file is no candidate
	writeBackup(t, binDir, "bd.abcdef1", 10*24*time.Hour, 7)
	writeBackup(t, binDir, "NOTES.txt", 1*time.Hour, 3)

	got, err := NewTarget(Config{ConfigDirs: []string{cfgDir}, BinaryDirs: []string{binDir}}).
		Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if want := int64(37); got.Used != want {
		t.Errorf("Used = %d, want %d", got.Used, want)
	}
	for _, part := range []string{"3 backup candidates", "37 bytes"} {
		if !strings.Contains(got.Detail, part) {
			t.Errorf("Detail = %q, want it to hold %q", got.Detail, part)
		}
	}
}

func TestTargetPlanRefusesWithoutALsofRunner(t *testing.T) {
	t.Parallel()
	plan, err := NewTarget(Config{ConfigDirs: []string{t.TempDir()}}).
		Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.Refused, "no lsof runner") {
		t.Errorf("Refused = %q, want it to name the missing runner", plan.Refused)
	}
}

func TestTargetPlanRefusesWithoutDirectories(t *testing.T) {
	t.Parallel()
	plan, err := NewTarget(Config{Run: newFakeLsof(t).run}).
		Plan(context.Background(), diskmaint.Policy{Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.Refused, "no config or binary directory") {
		t.Errorf("Refused = %q, want it to name the missing directories", plan.Refused)
	}
	if _, err := NewTarget(Config{}).Measure(context.Background()); err == nil {
		t.Error("Measure with no directories = nil, want a refusal")
	}
}

// oneDirConfig is one Config per directory kind, so a test covers the config
// loop and the binary loop without repeating the fixture.
var oneDirConfig = map[string]func(dir string) Config{
	"config directory": func(dir string) Config { return Config{ConfigDirs: []string{dir}} },
	"binary directory": func(dir string) Config { return Config{BinaryDirs: []string{dir}} },
}

func TestTargetPlanStopsWhenTheContextIsCanceled(t *testing.T) {
	t.Parallel()
	for name, config := range oneDirConfig {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeBackup(t, dir, "app.toml.bak-01", 40*24*time.Hour, 10)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			cfg := config(dir)
			cfg.Run = newFakeLsof(t).run
			if _, err := NewTarget(cfg).Plan(ctx, diskmaint.Policy{Now: testNow}); !errors.Is(err, context.Canceled) {
				t.Errorf("Plan with a canceled context = %v, want context.Canceled", err)
			}
		})
	}
}

func TestTargetPlanStopsWhenTheOpenCheckIsCanceled(t *testing.T) {
	t.Parallel()
	for name, config := range oneDirConfig {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cand := writeBackup(t, dir, "app.toml.bak-01", 40*24*time.Hour, 10)
			ctx, cancel := context.WithCancel(context.Background())
			lsof := newFakeLsof(t)
			lsof.cancel = cancel
			lsof.failed(cand, 2)

			cfg := config(dir)
			cfg.Run = lsof.run
			if _, err := NewTarget(cfg).Plan(ctx, diskmaint.Policy{Now: testNow}); !errors.Is(err, context.Canceled) {
				t.Errorf("Plan over a canceled lsof = %v, want context.Canceled", err)
			}
		})
	}
}
