package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

const cutoverStamp = "20261106-1430"

// cutoverDoctorFake answers install's two checks without Docker, so the CLI's
// wiring is what these tests drive.
type cutoverDoctorFake struct {
	inImage   error
	installed error
}

func (d cutoverDoctorFake) InImage(string) error           { return d.inImage }
func (d cutoverDoctorFake) Installed(string, string) error { return d.installed }

// memFiles is the in-memory filesystem the CLI's cutover changes. Its Rename
// refuses a destination that already exists, so a run that succeeds also proves
// no step wrote over a file.
type memFiles struct {
	files map[string]memFile
	dirs  map[string]bool
	next  int
}

type memFile struct {
	data string
	mode fs.FileMode
}

func newMemFiles() *memFiles {
	return &memFiles{files: map[string]memFile{}, dirs: map[string]bool{}}
}

func (f *memFiles) add(path, data string, mode fs.FileMode) {
	f.files[path] = memFile{data: data, mode: mode}
}

// data is a file's contents, or "" when the file is absent.
func (f *memFiles) data(path string) string { return f.files[path].data }

func (f *memFiles) Stat(path string) (fs.FileInfo, error) {
	if file, ok := f.files[path]; ok {
		return memInfo{name: filepath.Base(path), mode: file.mode, size: int64(len(file.data))}, nil
	}
	if f.dirs[path] {
		return memInfo{name: filepath.Base(path), mode: fs.ModeDir | 0o755}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

func (f *memFiles) ReadFile(path string) ([]byte, error) {
	file, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return []byte(file.data), nil
}

func (f *memFiles) CreateTemp(dir, pattern string) (gtwrap.TempFile, error) {
	f.next++
	name := filepath.Join(dir, strings.Replace(pattern, "*", fmt.Sprint(f.next), 1))
	return &memTemp{files: f, name: name, mode: 0o600}, nil
}

func (f *memFiles) Rename(oldpath, newpath string) error {
	file, ok := f.files[oldpath]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	if _, exists := f.files[newpath]; exists {
		return fmt.Errorf("rename %s over the existing %s", oldpath, newpath)
	}
	delete(f.files, oldpath)
	f.files[newpath] = file
	return nil
}

func (f *memFiles) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
	}
	delete(f.files, path)
	return nil
}

// memTemp is the new file place writes the wrapper into; it appears in the
// filesystem when it is closed.
type memTemp struct {
	files *memFiles
	name  string
	mode  fs.FileMode
	buf   bytes.Buffer
}

func (t *memTemp) Write(p []byte) (int, error) { return t.buf.Write(p) }
func (t *memTemp) Name() string                { return t.name }
func (t *memTemp) Chmod(mode fs.FileMode) error {
	t.mode = mode
	return nil
}

func (t *memTemp) Close() error {
	t.files.add(t.name, t.buf.String(), t.mode)
	return nil
}

// memInfo is the fs.FileInfo memFiles answers with.
type memInfo struct {
	name string
	mode fs.FileMode
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return i.mode }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.mode.IsDir() }
func (i memInfo) Sys() any           { return nil }

func TestCLIRunInstallPlacesTheWrapperOverTheNamedTools(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	targets := map[string]string{}
	for _, name := range []string{"gt", "bd"} {
		targets[name] = filepath.Join("/tools", name)
		h.files.add(targets[name], "old "+name, 0o755)
	}
	const source = "/src/gt-wrap"
	h.files.add(source, "the wrapper", 0o755)
	h.surface.executable = func() (string, error) { return source, nil }
	h.surface.newDoctor = func(town string) gtwrap.Doctor {
		if town != h.town {
			t.Errorf("built the doctor for town %q, want %q", town, h.town)
		}
		return cutoverDoctorFake{}
	}

	exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp,
		"--target", "gt="+targets["gt"], "--target", "bd="+targets["bd"])
	if exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	for _, name := range []string{"gt", "bd"} {
		if got := h.files.data(targets[name]); got != "the wrapper" {
			t.Errorf("%s holds %q, want the wrapper", targets[name], got)
		}
		if got := h.files.data(targets[name] + ".pre-wrap-" + cutoverStamp); got != "old "+name {
			t.Errorf("%s holds %q, want the file it replaced", targets[name], got)
		}
	}
	if !strings.Contains(h.stdout.String(), "rollback: gt-wrap rollback --stamp '"+cutoverStamp+"'") {
		t.Errorf("install did not print the rollback command:\n%s", h.stdout)
	}
}

func TestCLIRunRollbackRestoresTheFilesWithoutATown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return "", errors.New("no working directory") }
	h.surface.getenv = func(string) string { return "" }
	path := "/tools/gt"
	h.files.add(path, "the wrapper", 0o755)
	h.files.add(path+".pre-wrap-"+cutoverStamp, "old gt", 0o755)

	exit := h.run("gt-wrap", "rollback", "--stamp", cutoverStamp, "--target", "gt="+path)
	if exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if got := h.files.data(path); got != "old gt" {
		t.Errorf("%s holds %q, want the dated file back", path, got)
	}
	if got := h.files.data(path + ".wrapper-" + cutoverStamp); got != "the wrapper" {
		t.Errorf("the wrapper was not kept aside: %q", got)
	}
	if !strings.Contains(h.stdout.String(), "restored "+path) {
		t.Errorf("rollback did not print what it restored:\n%s", h.stdout)
	}
}

func TestCLIRunRefusesACutoverCommandItDoesNotKnow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "upgrade"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	for _, want := range []string{`"upgrade"`, "install and rollback"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not name %q", h.stderr, want)
		}
	}
}

func TestCLIRunSendsInstallToTheToolWhenInvokedAsOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt", "install"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.fake.launched {
		t.Fatal("gt install did not reach the tool")
	}
	if want := []string{"gt", "install"}; !slices.Equal(h.fake.call.Argv, want) {
		t.Fatalf("launched %q, want %q", h.fake.call.Argv, want)
	}
}

func TestCLIRunInstallRefusesWithoutAStampBeforeChangingFiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	path := "/tools/gt"
	h.files.add(path, "old gt", 0o755)
	h.surface.executable = func() (string, error) { return "/src/gt-wrap", nil }
	h.surface.newDoctor = func(string) gtwrap.Doctor { return cutoverDoctorFake{} }

	if exit := h.run("gt-wrap", "install", "--dir", "/tools"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "stamp") {
		t.Errorf("stderr %q does not name the missing stamp", h.stderr)
	}
	if got := h.files.data(path); got != "old gt" {
		t.Errorf("%s holds %q, want the file it had before the run", path, got)
	}
}

func TestCLIRunRefusesBothADirectoryAndTargets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--dir", "/tools", "--target", "gt=/bin/gt")
	if exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "--dir and --target") {
		t.Errorf("stderr %q does not say the two flags conflict", h.stderr)
	}
}

func TestCLIRunInstallRefusesAToolTheWrapperDoesNotAnswerAs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	path := "/tools/bash"
	h.files.add(path, "a shell", 0o755)
	h.surface.executable = func() (string, error) { return "/src/gt-wrap", nil }
	h.surface.newDoctor = func(string) gtwrap.Doctor { return cutoverDoctorFake{} }

	exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--target", "bash="+path)
	if exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "gt, bd, om and hm") {
		t.Errorf("stderr %q does not name the tools the wrapper answers as", h.stderr)
	}
	if got := h.files.data(path); got != "a shell" {
		t.Errorf("%s holds %q, want the file it had before the run", path, got)
	}
}

func TestCLIRunInstallRefusesWithoutATown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
	h.surface.getenv = func(string) string { return "" }
	h.surface.executable = func() (string, error) { return "/src/gt-wrap", nil }
	h.surface.newDoctor = func(string) gtwrap.Doctor { return cutoverDoctorFake{} }

	if exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--dir", "/tools"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "no town root") {
		t.Errorf("stderr %q does not say no town root was found", h.stderr)
	}
}

// TestCLIRunDoctorAnswersThroughTheWrapperName: gt-wrap --doctor is the check
// install makes before the cutover, so the operator can run it first.
func TestCLIRunDoctorAnswersThroughTheWrapperName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.diag = gtwrap.Diagnosis{
		Rung:      gtwrap.RungExec,
		Container: "gt-daemon",
		Image:     "gt-daemon:7f3e9a1",
		Reason:    "the daemon container gt-daemon is running",
	}
	if exit := h.run("gt-wrap", doctorFlag); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.fake.doctored {
		t.Fatal("gt-wrap --doctor never asked the target's doctor")
	}
	if !strings.Contains(h.stdout.String(), "rung: exec") {
		t.Errorf("report = %q, want the ladder", h.stdout)
	}
}

// cutoverShell answers the docker probes cutoverDoctor.InImage makes, so the
// doctor's wiring is checked without Docker.
type cutoverShell struct {
	captured [][]string
}

func (s *cutoverShell) Attach(argv []string) (int, error) { return 0, nil }

func (s *cutoverShell) Capture(ctx context.Context, argv []string) (string, int, error) {
	s.captured = append(s.captured, argv)
	if len(argv) < 2 {
		return "", 1, nil
	}
	switch argv[1] {
	case "info":
		return "", 0, nil
	case "ps":
		return "5c1e0a7f\n", 0, nil
	case "exec":
		return "/usr/local/bin/gt\n", 0, nil
	}
	return "", 1, nil
}

func TestCLICutoverDoctorChecksTheImageOfItsTown(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  daemon:\n    image: \"gt-daemon:7f3e9a1\"\n    container_name: \"gt-daemon\"\n"
	shell := &cutoverShell{}
	doctor := cutoverDoctor{
		town:  "/Users/operator/gt",
		shell: shell,
		read: func(path string) ([]byte, error) {
			if path != "/Users/operator/gt/daemon/container/compose.yml" {
				return nil, errors.New("the compose file was read at " + path)
			}
			return []byte(compose), nil
		},
	}
	if err := doctor.InImage("gt"); err != nil {
		t.Fatalf("InImage(gt) = %v, want nil", err)
	}
	want := []string{"docker", "exec", "gt-daemon", "sh", "-c", `command -v "$1"`, "sh", "gt"}
	if len(shell.captured) != 3 || !slices.Equal(shell.captured[2], want) {
		t.Fatalf("docker lines = %q, want the probes then %q", shell.captured, want)
	}
}

// TestCLICutoverDoctorRunsThePlacedFileWithDoctor: install's doctor runs the file
// it placed with --doctor, so the check is the placed tool's own answer.
func TestCLICutoverDoctorRunsThePlacedFileWithDoctor(t *testing.T) {
	t.Parallel()
	const path = "/Users/operator/.local/bin/gt"
	var ran []string
	doctor := cutoverDoctor{run: func(argv ...string) error {
		ran = argv
		return nil
	}}
	if err := doctor.Installed("gt", path); err != nil {
		t.Fatalf("Installed = %v, want nil", err)
	}
	if want := []string{path, doctorFlag}; !slices.Equal(ran, want) {
		t.Fatalf("ran %q, want %q", ran, want)
	}
}

func TestCLICutoverDoctorReportsAFailedInstalledFile(t *testing.T) {
	t.Parallel()
	const path = "/Users/operator/.local/bin/gt"
	doctor := cutoverDoctor{run: func(...string) error { return errors.New("exit status 1") }}
	err := doctor.Installed("gt", path)
	if err == nil || !strings.Contains(err.Error(), "gt --doctor at "+path) {
		t.Fatalf("Installed = %v, want the failed doctor named", err)
	}
}

func TestCLIRunRefusesACutoverFlagItDoesNotKnow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "install", "--no-such-flag"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "install") {
		t.Errorf("stderr %q does not name the command", h.stderr)
	}
}

func TestCLIRunRefusesAnArgumentAfterTheFlags(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "rollback", "extra"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "unexpected argument") {
		t.Errorf("stderr %q does not name the stray argument", h.stderr)
	}
}

func TestCLIRunRefusesATargetThatIsNotNameEqualsPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--target", "gt"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "not name=path") {
		t.Errorf("stderr %q does not say the target is malformed", h.stderr)
	}
}

func TestCLIRunInstallRefusesWhenTheWrapperCannotBeLocated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.executable = func() (string, error) { return "", errors.New("no executable") }
	if exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--dir", "/tools"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "wrapper binary") {
		t.Errorf("stderr %q does not say the wrapper binary was not found", h.stderr)
	}
}

func TestCLIRunRefusesAnEmptyCutoverCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	for _, want := range []string{"install", "rollback"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr %q does not name %q", h.stderr, want)
		}
	}
}

func TestCLIRunRollbackReportsWhatItCouldNotRestore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	path := "/tools/gt"
	h.files.add(path, "the wrapper", 0o755)
	if exit := h.run("gt-wrap", "rollback", "--stamp", cutoverStamp, "--target", "gt="+path); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), path+".pre-wrap-"+cutoverStamp) {
		t.Errorf("stderr %q does not name the missing dated file", h.stderr)
	}
}
