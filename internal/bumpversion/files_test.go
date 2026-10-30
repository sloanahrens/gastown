package bumpversion

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memFS is the in-memory FS seam. A name that is not in files reads as
// fs.ErrNotExist, and every write lands in files.
type memFS struct{ files map[string]string }

func (m *memFS) ReadFile(name string) ([]byte, error) {
	text, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(text), nil
}

func (m *memFS) WriteFile(name string, data []byte) error {
	m.files[name] = string(data)
	return nil
}

// fixture returns the named file from testdata/, failing the test when it cannot
// be read.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// TestCurrentVersionTable: the version is the quoted value on the Version = line,
// and a file with no such line, or no quoted value on it, gives "".
func TestCurrentVersionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "golden version.go", text: fixture(t, "version.go.before"), want: "1.2.1"},
		{name: "no Version line", text: "package cmd\n", want: ""},
		{name: "unquoted value", text: "Version = 1.2.1\n", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CurrentVersion(tc.text); got != tc.want {
				t.Errorf("CurrentVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetFunctionsMatchGoldenFixtures: each Set function output is its golden
// file byte for byte. The golden files are the script's own sed substitutions
// run on the fixtures.
func TestSetFunctionsMatchGoldenFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		before string
		after  string
		set    func(text, oldVersion, newVersion string) string
	}{
		{name: "version.go", before: "version.go.before", after: "version.go.after", set: SetVersionGo},
		{name: "package.json", before: "package.json.before", after: "package.json.after", set: SetPackageJSON},
		{name: "flake.nix", before: "flake.nix.before", after: "flake.nix.after", set: SetFlakeVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.set(fixture(t, tc.before), "1.2.1", "1.3.0")
			if want := fixture(t, tc.after); got != want {
				t.Errorf("%s output differs from golden %s\n--- got ---\n%s\n--- want ---\n%s", tc.name, tc.after, got, want)
			}
		})
	}
}

// TestSetReplacesEveryOccurrence: the flake fixture holds the version on two
// lines, and both change, as the script's global substitution does.
func TestSetReplacesEveryOccurrence(t *testing.T) {
	t.Parallel()
	got := SetFlakeVersion(fixture(t, "flake.nix.before"), "1.2.1", "1.3.0")
	if n := strings.Count(got, `version = "1.3.0"`); n != 2 {
		t.Errorf("flake.nix has %d lines at 1.3.0 after the edit, want 2", n)
	}
	if strings.Contains(got, `"1.2.1"`) {
		t.Errorf("flake.nix still holds 1.2.1 after the edit:\n%s", got)
	}
}

// TestSetEditsFileThroughSeam: EditFile reads the file through the seam, applies
// the Set function and writes it back. A missing file is an error.
func TestSetEditsFileThroughSeam(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: map[string]string{PackageJSONPath: fixture(t, "package.json.before")}}
	edit := func(text string) string { return SetPackageJSON(text, "1.2.1", "1.3.0") }
	if err := EditFile(fsys, PackageJSONPath, edit); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	if got, want := fsys.files[PackageJSONPath], fixture(t, "package.json.after"); got != want {
		t.Errorf("package.json after EditFile differs from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if err := EditFile(&memFS{files: map[string]string{}}, FlakePath, edit); err == nil {
		t.Errorf("EditFile on a missing file returned nil, want an error")
	}
}

// TestUpdateChangelogCases: the heading moves to the new entry in place. A
// missing CHANGELOG.md and one with no [Unreleased] heading are warnings, and
// neither is written.
func TestUpdateChangelogCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		file        string // testdata name of CHANGELOG.md; "" means the file is absent
		wantFile    string // testdata name of CHANGELOG.md afterwards; "" means it is absent
		wantWarning string
	}{
		{name: "normal", file: "CHANGELOG.md.before", wantFile: "CHANGELOG.md.after"},
		{
			name:        "no Unreleased section",
			file:        "CHANGELOG.md.nounreleased",
			wantFile:    "CHANGELOG.md.nounreleased",
			wantWarning: "Warning: No [Unreleased] section in CHANGELOG.md\nYou may need to manually update CHANGELOG.md",
		},
		{name: "missing file", wantWarning: "Warning: CHANGELOG.md not found, skipping"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{}
			if tc.file != "" {
				files[ChangelogPath] = fixture(t, tc.file)
			}
			fsys := &memFS{files: files}
			warning, err := UpdateChangelogFile(fsys, "1.3.0", "2026-10-30")
			if err != nil {
				t.Fatalf("UpdateChangelogFile: %v", err)
			}
			if warning != tc.wantWarning {
				t.Errorf("warning = %q, want %q", warning, tc.wantWarning)
			}
			got, present := fsys.files[ChangelogPath]
			if tc.wantFile == "" {
				if present {
					t.Errorf("CHANGELOG.md was written, want it absent")
				}
				return
			}
			if want := fixture(t, tc.wantFile); got != want {
				t.Errorf("CHANGELOG.md differs from golden %s\n--- got ---\n%s\n--- want ---\n%s", tc.wantFile, got, want)
			}
		})
	}
}

// readErrFS is the FS seam whose reads fail with err, a failure other than a
// missing file.
type readErrFS struct{ err error }

func (f readErrFS) ReadFile(string) ([]byte, error) { return nil, f.err }

func (f readErrFS) WriteFile(string, []byte) error { return errors.New("write must not run") }

// TestUpdateChangelogReadErrorFails: a CHANGELOG.md that cannot be read for a
// reason other than being absent is an error, not a warning, and the cause is
// kept for the caller.
func TestUpdateChangelogReadErrorFails(t *testing.T) {
	t.Parallel()
	cause := errors.New("permission denied")
	warning, err := UpdateChangelogFile(readErrFS{err: cause}, "1.3.0", "2026-10-30")
	if !errors.Is(err, cause) {
		t.Fatalf("UpdateChangelogFile error = %v, want it to wrap %v", err, cause)
	}
	if warning != "" {
		t.Errorf("warning = %q, want none on a read error", warning)
	}
}

// goldenTree returns a tree holding the three version files at 1.3.0, as the
// script leaves them after a bump.
func goldenTree(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		VersionGoPath:   fixture(t, "version.go.after"),
		PackageJSONPath: fixture(t, "package.json.after"),
		FlakePath:       fixture(t, "flake.nix.after"),
	}
}

// TestVerifyMatchesAllFiles: a tree at the wanted version verifies with nix.
func TestVerifyMatchesAllFiles(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: goldenTree(t)}
	if err := Verify(fsys, "1.3.0", true); err != nil {
		t.Errorf("Verify(1.3.0, nix) = %v, want nil", err)
	}
}

// TestVerifyMismatchReportsEachFile: a mismatch reports the version every file
// holds, not only the first one that differs. flake.nix is in the report only
// when nix is true.
func TestVerifyMismatchReportsEachFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		nix  bool
		want string
	}{
		{
			name: "nix",
			nix:  true,
			want: "Version mismatch detected!\n  version.go: 1.3.0\n  package.json: 1.2.1\n  flake.nix: 1.2.1",
		},
		{
			name: "no nix",
			nix:  false,
			want: "Version mismatch detected!\n  version.go: 1.3.0\n  package.json: 1.2.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := &memFS{files: map[string]string{
				VersionGoPath:   fixture(t, "version.go.after"),
				PackageJSONPath: fixture(t, "package.json.before"),
				FlakePath:       fixture(t, "flake.nix.before"),
			}}
			err := Verify(fsys, "1.3.0", tc.nix)
			var mismatch *MismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("Verify = %v, want a *MismatchError", err)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("Verify message = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifySkipsNixWhenAbsent: with nix false, flake.nix is not read, so a tree
// with no flake.nix verifies.
func TestVerifySkipsNixWhenAbsent(t *testing.T) {
	t.Parallel()
	files := goldenTree(t)
	delete(files, FlakePath)
	if err := Verify(&memFS{files: files}, "1.3.0", false); err != nil {
		t.Errorf("Verify(1.3.0, no nix) with no flake.nix = %v, want nil", err)
	}
}

// TestVerifyReadErrorFailsClosed: a file that cannot be read is an error, never
// a pass, and the error keeps the cause.
func TestVerifyReadErrorFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		missing string
		nix     bool
	}{
		{name: "missing version.go", missing: VersionGoPath, nix: true},
		{name: "missing package.json", missing: PackageJSONPath, nix: true},
		{name: "missing flake.nix with nix", missing: FlakePath, nix: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := goldenTree(t)
			delete(files, tc.missing)
			err := Verify(&memFS{files: files}, "1.3.0", tc.nix)
			if err == nil {
				t.Fatalf("Verify with %s returned nil, want an error", tc.missing)
			}
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Verify error = %v, want it to wrap fs.ErrNotExist", err)
			}
		})
	}
}

// TestVerifyRefusesInvalidWant: an empty want cannot match files that hold no
// version, so Verify refuses it before reading anything.
func TestVerifyRefusesInvalidWant(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: map[string]string{
		VersionGoPath:   "package cmd\n",
		PackageJSONPath: "{}\n",
	}}
	if err := Verify(fsys, "", false); err == nil {
		t.Errorf("Verify with an empty want returned nil, want an error")
	}
}
