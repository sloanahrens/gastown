package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestRunInstallPlacesTheWrapperOverTheNamedTools(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dir := t.TempDir()
	targets := map[string]string{}
	for _, name := range []string{"gt", "bd"} {
		targets[name] = filepath.Join(dir, name)
		writeFile(t, targets[name], "old "+name)
	}
	source := filepath.Join(t.TempDir(), "gt-wrap")
	writeFile(t, source, "the wrapper")
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
		if got := readFile(t, targets[name]); got != "the wrapper" {
			t.Errorf("%s holds %q, want the wrapper", targets[name], got)
		}
		if got := readFile(t, targets[name]+".pre-wrap-"+cutoverStamp); got != "old "+name {
			t.Errorf("%s holds %q, want the file it replaced", targets[name], got)
		}
	}
	if !strings.Contains(h.stdout.String(), "rollback: gt-wrap rollback --stamp '"+cutoverStamp+"'") {
		t.Errorf("install did not print the rollback command:\n%s", h.stdout)
	}
}

func TestRunRollbackRestoresTheFilesWithoutATown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return "", errors.New("no working directory") }
	h.surface.getenv = func(string) string { return "" }
	dir := t.TempDir()
	path := filepath.Join(dir, "gt")
	writeFile(t, path, "the wrapper")
	writeFile(t, path+".pre-wrap-"+cutoverStamp, "old gt")

	exit := h.run("gt-wrap", "rollback", "--stamp", cutoverStamp, "--target", "gt="+path)
	if exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if got := readFile(t, path); got != "old gt" {
		t.Errorf("%s holds %q, want the dated file back", path, got)
	}
	if got := readFile(t, path+".wrapper-"+cutoverStamp); got != "the wrapper" {
		t.Errorf("the wrapper was not kept aside: %q", got)
	}
	if !strings.Contains(h.stdout.String(), "restored "+path) {
		t.Errorf("rollback did not print what it restored:\n%s", h.stdout)
	}
}

func TestRunRefusesACutoverCommandItDoesNotKnow(t *testing.T) {
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

func TestRunSendsInstallToTheToolWhenInvokedAsOne(t *testing.T) {
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

func TestRunInstallRefusesWithoutAStampBeforeChangingFiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gt")
	writeFile(t, path, "old gt")
	h.surface.executable = func() (string, error) { return filepath.Join(t.TempDir(), "gt-wrap"), nil }
	h.surface.newDoctor = func(string) gtwrap.Doctor { return cutoverDoctorFake{} }

	if exit := h.run("gt-wrap", "install", "--dir", dir); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "stamp") {
		t.Errorf("stderr %q does not name the missing stamp", h.stderr)
	}
	if got := readFile(t, path); got != "old gt" {
		t.Errorf("%s holds %q, want the file it had before the run", path, got)
	}
}

func TestRunRefusesBothADirectoryAndTargets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--dir", t.TempDir(), "--target", "gt=/bin/gt")
	if exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "--dir and --target") {
		t.Errorf("stderr %q does not say the two flags conflict", h.stderr)
	}
}

func TestRunInstallRefusesAToolTheWrapperDoesNotAnswerAs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bash")
	writeFile(t, path, "a shell")
	h.surface.executable = func() (string, error) { return filepath.Join(t.TempDir(), "gt-wrap"), nil }
	h.surface.newDoctor = func(string) gtwrap.Doctor { return cutoverDoctorFake{} }

	exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--target", "bash="+path)
	if exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "gt, bd, om and hm") {
		t.Errorf("stderr %q does not name the tools the wrapper answers as", h.stderr)
	}
	if got := readFile(t, path); got != "a shell" {
		t.Errorf("%s holds %q, want the file it had before the run", path, got)
	}
}

func TestRunInstallRefusesWithoutATown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
	h.surface.getenv = func(string) string { return "" }
	h.surface.executable = func() (string, error) { return filepath.Join(t.TempDir(), "gt-wrap"), nil }
	h.surface.newDoctor = func(string) gtwrap.Doctor { return cutoverDoctorFake{} }

	if exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--dir", t.TempDir()); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "no town root") {
		t.Errorf("stderr %q does not say no town root was found", h.stderr)
	}
}

// TestRunDoctorAnswersThroughTheWrapperName: gt-wrap --doctor is the check
// install makes before the cutover, so the operator can run it first.
func TestRunDoctorAnswersThroughTheWrapperName(t *testing.T) {
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

func TestCutoverDoctorChecksTheImageOfItsTown(t *testing.T) {
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

func TestCutoverDoctorReportsAFailedInstalledFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gt")
	err := cutoverDoctor{report: &bytes.Buffer{}}.Installed("gt", path)
	if err == nil || !strings.Contains(err.Error(), "gt --doctor at "+path) {
		t.Fatalf("Installed = %v, want the failed doctor named", err)
	}
}

func TestRunRefusesACutoverFlagItDoesNotKnow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "install", "--no-such-flag"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "install") {
		t.Errorf("stderr %q does not name the command", h.stderr)
	}
}

func TestRunRefusesAnArgumentAfterTheFlags(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "rollback", "extra"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "unexpected argument") {
		t.Errorf("stderr %q does not name the stray argument", h.stderr)
	}
}

func TestRunRefusesATargetThatIsNotNameEqualsPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--target", "gt"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "not name=path") {
		t.Errorf("stderr %q does not say the target is malformed", h.stderr)
	}
}

func TestRunInstallRefusesWhenTheWrapperCannotBeLocated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.surface.executable = func() (string, error) { return "", errors.New("no executable") }
	if exit := h.run("gt-wrap", "install", "--stamp", cutoverStamp, "--dir", t.TempDir()); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "wrapper binary") {
		t.Errorf("stderr %q does not say the wrapper binary was not found", h.stderr)
	}
}

func TestRunRefusesAnEmptyCutoverCommand(t *testing.T) {
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

func TestRunRollbackReportsWhatItCouldNotRestore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "gt")
	writeFile(t, path, "the wrapper")
	if exit := h.run("gt-wrap", "rollback", "--stamp", cutoverStamp, "--target", "gt="+path); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), path+".pre-wrap-"+cutoverStamp) {
		t.Errorf("stderr %q does not name the missing dated file", h.stderr)
	}
}
