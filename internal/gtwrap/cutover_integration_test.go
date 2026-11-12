//go:build integration

package gtwrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIntegrationInstallPlacesARealCopy installs a stub wrapper into a
// temporary directory over real files, with the operating system's filesystem:
// the one place the copy's temp file and renames run on real paths.
func TestIntegrationInstallPlacesARealCopy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "gt-wrap")
	stub := []byte("#!/bin/sh\necho stub wrapper\n")
	if err := os.WriteFile(source, stub, 0o755); err != nil {
		t.Fatalf("write the stub wrapper: %v", err)
	}
	old := []byte("the tool it replaces\n")
	names := []string{"gt", "bd", "om", "hm"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), old, 0o755); err != nil {
			t.Fatalf("write the %s it replaces: %v", name, err)
		}
	}

	var out strings.Builder
	if err := Install(InstallConfig{Source: source, Stamp: testStamp, Dir: dir, Doctor: &fakeDoctor{}, Out: &out}); err != nil {
		t.Fatalf("Install: %v\n%s", err, out.String())
	}

	for _, name := range names {
		path := filepath.Join(dir, name)
		placed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the placed %s: %v", name, err)
		}
		if string(placed) != string(stub) {
			t.Errorf("%s holds %q, want the stub wrapper", path, placed)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat the placed %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %v, want the stub's 0755", path, info.Mode().Perm())
		}
		kept, err := os.ReadFile(dated(Target{Name: name, Path: path}, testStamp))
		if err != nil {
			t.Fatalf("read the kept %s: %v", name, err)
		}
		if string(kept) != string(old) {
			t.Errorf("%s holds %q, want the file it replaced", name, kept)
		}
	}

	// The copy runs: the placed file is executable and is not written through.
	run, err := exec.Command(filepath.Join(dir, "gt")).Output()
	if err != nil {
		t.Fatalf("run the placed wrapper: %v", err)
	}
	if strings.TrimSpace(string(run)) != "stub wrapper" {
		t.Errorf("the placed wrapper printed %q", run)
	}
}

// TestIntegrationRollbackPutsARealFileBack restores a real dated file and keeps
// the wrapper it replaced aside.
func TestIntegrationRollbackPutsARealFileBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gt")
	if err := os.WriteFile(path, []byte("the wrapper\n"), 0o755); err != nil {
		t.Fatalf("write the wrapper: %v", err)
	}
	kept := dated(Target{Name: "gt", Path: path}, testStamp)
	if err := os.WriteFile(kept, []byte("the tool it replaced\n"), 0o755); err != nil {
		t.Fatalf("write the dated file: %v", err)
	}

	var out strings.Builder
	err := Rollback(RollbackConfig{Stamp: testStamp, Targets: []Target{{Name: "gt", Path: path}}, Out: &out})
	if err != nil {
		t.Fatalf("Rollback: %v\n%s", err, out.String())
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != "the tool it replaced\n" {
		t.Fatalf("%s holds %q, %v; want the dated file back", path, got, err)
	}
	if got, err := os.ReadFile(aside(Target{Name: "gt", Path: path}, testStamp)); err != nil || string(got) != "the wrapper\n" {
		t.Fatalf("the wrapper beside %s holds %q, %v; want it kept aside", path, got, err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Errorf("%s still exists after the restore", kept)
	}
}

// TestIntegrationInstallExplainsAFlaggedFile installs over a real file that
// carries the macOS immutable flag, as the operator's gt and om did. The kernel
// refuses the rename, and install must say why and name the command that clears
// the flag, with the file untouched. The flag is set and cleared with the
// system's own chflags; it skips where that tool or the flag is not there.
func TestIntegrationInstallExplainsAFlaggedFile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the immutable flag install explains is the Mac's")
	}
	chflags, err := exec.LookPath("chflags")
	if err != nil {
		t.Skip("no chflags on this host")
	}
	dir := t.TempDir()
	gt := filepath.Join(dir, "gt")
	if err := os.WriteFile(gt, []byte("old gt\n"), 0o755); err != nil {
		t.Fatalf("write the old gt: %v", err)
	}
	if out, err := exec.Command(chflags, "uchg", gt).CombinedOutput(); err != nil { //nolint:gosec // G204: chflags is the system tool and gt a temp file
		t.Skipf("the host refuses the immutable flag: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(chflags, "nouchg", gt).Run() }) //nolint:gosec // G204: as above
	source := filepath.Join(t.TempDir(), "gt-wrap")
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho stub wrapper\n"), 0o755); err != nil {
		t.Fatalf("write the stub wrapper: %v", err)
	}

	var out strings.Builder
	err = Install(InstallConfig{Source: source, Stamp: testStamp, Targets: []Target{{Name: "gt", Path: gt}}, Doctor: &fakeDoctor{}, Out: &out})
	if err == nil {
		t.Fatalf("Install over a flagged file succeeded:\n%s", out.String())
	}
	for _, want := range []string{"immutable flag", "chflags nouchg", "nothing was changed for gt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}
	if got, _ := os.ReadFile(gt); string(got) != "old gt\n" {
		t.Errorf("%s holds %q, want it untouched", gt, got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("%s holds %d entries after a refused install, want only gt", dir, len(entries))
	}
}
