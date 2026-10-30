package bumpversion

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// oldVendorHash is the vendorHash in testdata/flake.nix.before.
const oldVendorHash = "sha256-mJzpsl4XnIm3ZSg7fFn0MOdQQW1bdOkAJ+TikiLMXJM="

// newVendorHash is what the fake nix hash path prints. It has the SRI sha256 shape.
const newVendorHash = "sha256-EVB6Di9eadXfpApiob17buV+a82FxnybhDGzb/8hxDc="

// fakeCall is one command the fake process seam was asked to run.
type fakeCall struct {
	dir  string
	env  []string
	name string
	args []string
}

// fakeExec is the process and scratch-directory seam with scripted results.
// failVendor leading go mod vendor calls fail, and nix hash path prints nixOut and
// writes nixErrOut to standard error, then fails with nixErr. A fake with hasNix
// false has no nix on PATH. Scratch directories are made in temp, and tempErr makes
// MkdirTemp fail instead.
type fakeExec struct {
	hasNix     bool
	failVendor int
	nixOut     string
	nixErrOut  string
	nixErr     error
	tempErr    error
	temp       string
	calls      []fakeCall
}

// newFakeExec returns the fake f with its scratch directories made in t.TempDir,
// so a directory the function leaves behind is seen by leftovers.
func newFakeExec(t *testing.T, f fakeExec) *fakeExec {
	t.Helper()
	f.temp = t.TempDir()
	return &f
}

func (f *fakeExec) exec() Exec {
	return Exec{
		Run: func(dir string, env []string, name string, args ...string) ([]byte, []byte, error) {
			f.calls = append(f.calls, fakeCall{dir: dir, env: env, name: name, args: args})
			switch {
			case name == "go" && f.failVendor > 0:
				f.failVendor--
				return nil, nil, errors.New("go mod vendor failed")
			case name == "nix":
				return []byte(f.nixOut), []byte(f.nixErrOut), f.nixErr
			}
			return nil, nil, nil
		},
		LookPath: func(file string) (string, error) {
			if !f.hasNix {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + file, nil
		},
		MkdirTemp: func(_, pattern string) (string, error) {
			if f.tempErr != nil {
				return "", f.tempErr
			}
			return os.MkdirTemp(f.temp, pattern)
		},
	}
}

// callsNamed returns the recorded calls whose command is name.
func (f *fakeExec) callsNamed(name string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// leftovers counts the entries the function left in the scratch directory.
func (f *fakeExec) leftovers(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(f.temp)
	if err != nil {
		t.Fatalf("read scratch dir: %v", err)
	}
	return len(entries)
}

// vendorDir returns the -o argument of a go mod vendor call.
func vendorDir(t *testing.T, c fakeCall) string {
	t.Helper()
	for i, a := range c.args {
		if a == "-o" && i+1 < len(c.args) {
			return c.args[i+1]
		}
	}
	t.Fatalf("go %v has no -o argument", c.args)
	return ""
}

// TestUpdateVendorHashReplacesTheHash: the fake nix returns a new hash, flake.nix
// holds it afterwards, and the old hash is gone.
func TestUpdateVendorHashReplacesTheHash(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: map[string]string{FlakePath: fixture(t, "flake.nix.before")}}
	fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: newVendorHash + "\n"})

	got, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard)
	if err != nil {
		t.Fatalf("UpdateVendorHash: %v", err)
	}
	if got.Old != oldVendorHash || got.New != newVendorHash {
		t.Errorf("result = %+v, want Old %q New %q", got, oldVendorHash, newVendorHash)
	}
	flake := fsys.files[FlakePath]
	if !strings.Contains(flake, `vendorHash = "`+newVendorHash+`"`) {
		t.Errorf("flake.nix does not hold the new hash:\n%s", flake)
	}
	if strings.Contains(flake, oldVendorHash) {
		t.Errorf("flake.nix still holds the old hash:\n%s", flake)
	}
	if n := fake.leftovers(t); n != 0 {
		t.Errorf("scratch dir has %d leftover entries, want none", n)
	}
}

// TestUpdateVendorHashUnchangedReportsUnchanged: when nix hashes the tree to the
// hash flake.nix already holds, the result says so and flake.nix is untouched.
func TestUpdateVendorHashUnchangedReportsUnchanged(t *testing.T) {
	t.Parallel()
	before := fixture(t, "flake.nix.before")
	fsys := &memFS{files: map[string]string{FlakePath: before}}
	fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: oldVendorHash + "\n"})

	got, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard)
	if err != nil {
		t.Fatalf("UpdateVendorHash: %v", err)
	}
	if got.Old != got.New || got.New != oldVendorHash {
		t.Errorf("result = %+v, want Old == New == %q", got, oldVendorHash)
	}
	if fsys.files[FlakePath] != before {
		t.Errorf("flake.nix changed although the hash did not")
	}
}

// TestUpdateVendorHashEmptyHashFails: a hash that is empty or blank is an error,
// and flake.nix is left as it was.
func TestUpdateVendorHashEmptyHashFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		nixOut string
	}{
		{name: "empty output", nixOut: ""},
		{name: "blank output", nixOut: " \n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := fixture(t, "flake.nix.before")
			fsys := &memFS{files: map[string]string{FlakePath: before}}
			fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: tc.nixOut})

			if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err == nil {
				t.Fatal("UpdateVendorHash succeeded, want an error")
			} else if err.Error() != "Could not compute vendorHash" {
				t.Errorf("error = %q, want %q", err, "Could not compute vendorHash")
			}
			if fsys.files[FlakePath] != before {
				t.Errorf("flake.nix changed on failure")
			}
		})
	}
}

// TestUpdateVendorHashRejectsMalformedOutput: output that is not one SRI sha256 hash
// is refused before the write. The error names the first 20 bytes of what nix
// printed, and flake.nix is left as it was.
func TestUpdateVendorHashRejectsMalformedOutput(t *testing.T) {
	t.Parallel()
	wrongPrefix := "sha512-" + strings.Repeat("A", 43) + "="
	shortHash := "sha256-" + strings.Repeat("A", 42) + "="
	tests := []struct {
		name   string
		nixOut string
		named  string
	}{
		{name: "two lines", nixOut: newVendorHash + "\n" + newVendorHash + "\n", named: newVendorHash[:20]},
		{name: "warning line plus hash", nixOut: "warning: cache unreadable\n" + newVendorHash + "\n", named: "warning: cache unrea"},
		{name: "wrong prefix", nixOut: wrongPrefix + "\n", named: wrongPrefix[:20]},
		{name: "wrong length", nixOut: shortHash + "\n", named: shortHash[:20]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := fixture(t, "flake.nix.before")
			fsys := &memFS{files: map[string]string{FlakePath: before}}
			fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: tc.nixOut})

			_, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard)
			if err == nil {
				t.Fatal("UpdateVendorHash succeeded, want an error")
			}
			if !strings.Contains(err.Error(), strconv.Quote(tc.named)) {
				t.Errorf("error = %q, want it to name %q", err, tc.named)
			}
			if fsys.files[FlakePath] != before {
				t.Errorf("flake.nix changed on malformed output:\n%s", fsys.files[FlakePath])
			}
		})
	}
}

// TestUpdateVendorHashNixFailureNamesStderr: when nix hash path fails, the error
// carries nix's standard error as one line, cut at 200 bytes, and flake.nix is
// left as it was.
func TestUpdateVendorHashNixFailureNamesStderr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{name: "multi-line stderr is one line", stderr: "error: builder failed\nsee the log\n", want: "error: builder failed see the log"},
		{name: "long stderr is cut at 200 bytes", stderr: strings.Repeat("e", 300), want: strings.Repeat("e", 200)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := fixture(t, "flake.nix.before")
			fsys := &memFS{files: map[string]string{FlakePath: before}}
			fake := newFakeExec(t, fakeExec{hasNix: true, nixErrOut: tc.stderr, nixErr: errors.New("exit status 1")})

			_, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard)
			if err == nil {
				t.Fatal("UpdateVendorHash succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), strings.Repeat("e", 201)) {
				t.Errorf("error = %q keeps more than 200 bytes of stderr", err)
			}
			if fsys.files[FlakePath] != before {
				t.Errorf("flake.nix changed on failure")
			}
		})
	}
}

// TestUpdateVendorHashVendorDirIsHashed: nix hashes the directory that go mod vendor
// wrote, and that directory is inside the scratch directory.
func TestUpdateVendorHashVendorDirIsHashed(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: map[string]string{FlakePath: fixture(t, "flake.nix.before")}}
	fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: newVendorHash})

	if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err != nil {
		t.Fatalf("UpdateVendorHash: %v", err)
	}
	goCalls := fake.callsNamed("go")
	nixCalls := fake.callsNamed("nix")
	if len(goCalls) == 0 || len(nixCalls) != 1 {
		t.Fatalf("go ran %d times and nix %d times, want at least one go and one nix", len(goCalls), len(nixCalls))
	}
	vendored := vendorDir(t, goCalls[len(goCalls)-1])
	hashed := nixCalls[0].args[len(nixCalls[0].args)-1]
	if vendored != hashed {
		t.Errorf("go mod vendor wrote %q but nix hashed %q", vendored, hashed)
	}
	if !strings.HasPrefix(vendored, fake.temp+string(filepath.Separator)) {
		t.Errorf("vendor dir %q is not under the scratch dir %q", vendored, fake.temp)
	}
}

// TestUpdateVendorHashNoVendorHashLineFails: a flake.nix with no vendorHash line
// is refused, since the replacement would find nothing and report success.
func TestUpdateVendorHashNoVendorHashLineFails(t *testing.T) {
	t.Parallel()
	before := "{\n  version = \"1.2.1\";\n}\n"
	fsys := &memFS{files: map[string]string{FlakePath: before}}
	fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: newVendorHash})

	if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err == nil {
		t.Fatal("UpdateVendorHash succeeded, want an error")
	}
	if fsys.files[FlakePath] != before {
		t.Errorf("flake.nix changed on failure")
	}
	if n := len(fake.callsNamed("nix")); n != 0 {
		t.Errorf("nix ran %d times, want none without a vendorHash line", n)
	}
}

// ioFS is an FS whose reads or writes fail with the errors set, over the
// in-memory files.
type ioFS struct {
	memFS
	readErr  error
	writeErr error
}

func (f *ioFS) ReadFile(name string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.memFS.ReadFile(name)
}

func (f *ioFS) WriteFile(name string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	return f.memFS.WriteFile(name, data)
}

// countingFS is an in-memory FS that counts its reads and writes, so a test can
// show that a step touched no file.
type countingFS struct {
	memFS
	reads  int
	writes int
}

func (f *countingFS) ReadFile(name string) ([]byte, error) {
	f.reads++
	return f.memFS.ReadFile(name)
}

func (f *countingFS) WriteFile(name string, data []byte) error {
	f.writes++
	return f.memFS.WriteFile(name, data)
}

// TestUpdateVendorHashIOErrorsFail: a flake.nix that cannot be read, a flake.nix
// that cannot be written, and a scratch directory that cannot be made are each an
// error. Where the vendor tree was made, its directory is removed again.
func TestUpdateVendorHashIOErrorsFail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		readErr  error
		writeErr error
		tempErr  error
	}{
		{name: "flake.nix unreadable", readErr: errors.New("permission denied")},
		{name: "flake.nix write fails", writeErr: errors.New("disk full")},
		{name: "scratch dir cannot be made", tempErr: errors.New("no space")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := &ioFS{
				memFS:    memFS{files: map[string]string{FlakePath: fixture(t, "flake.nix.before")}},
				readErr:  tc.readErr,
				writeErr: tc.writeErr,
			}
			fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: newVendorHash, tempErr: tc.tempErr})

			if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err == nil {
				t.Fatal("UpdateVendorHash succeeded, want an error")
			}
			if n := fake.leftovers(t); n != 0 {
				t.Errorf("scratch dir has %d leftover entries, want none", n)
			}
		})
	}
}

// TestGoModVendorRetriesWithoutTheEnvVar: the first go mod vendor fails under
// GOFLAGS=-mod=readonly, the second runs without that variable, and the hash
// comes from the tree the second run produced.
func TestGoModVendorRetriesWithoutTheEnvVar(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: map[string]string{FlakePath: fixture(t, "flake.nix.before")}}
	fake := newFakeExec(t, fakeExec{hasNix: true, failVendor: 1, nixOut: newVendorHash})

	if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err != nil {
		t.Fatalf("UpdateVendorHash: %v", err)
	}
	vendor := fake.callsNamed("go")
	if len(vendor) != 2 {
		t.Fatalf("go ran %d times, want 2", len(vendor))
	}
	if !containsEntry(vendor[0].env, "GOFLAGS=-mod=readonly") {
		t.Errorf("first attempt env = %v, want GOFLAGS=-mod=readonly", vendor[0].env)
	}
	if containsEntry(vendor[1].env, "GOFLAGS=-mod=readonly") {
		t.Errorf("retry env = %v, want no GOFLAGS", vendor[1].env)
	}
	if vendor[0].dir != "/repo" || vendor[1].dir != "/repo" {
		t.Errorf("go ran in %q and %q, want /repo", vendor[0].dir, vendor[1].dir)
	}
}

// TestGoModVendorFailsWhenRetryFails: when both go mod vendor attempts fail the
// error is returned, nix never hashes a missing tree, and flake.nix is untouched.
func TestGoModVendorFailsWhenRetryFails(t *testing.T) {
	t.Parallel()
	before := fixture(t, "flake.nix.before")
	fsys := &memFS{files: map[string]string{FlakePath: before}}
	fake := newFakeExec(t, fakeExec{hasNix: true, failVendor: 2, nixOut: newVendorHash})

	if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err == nil {
		t.Fatal("UpdateVendorHash succeeded, want an error")
	}
	if n := len(fake.callsNamed("nix")); n != 0 {
		t.Errorf("nix ran %d times, want none after go mod vendor failed", n)
	}
	if fsys.files[FlakePath] != before {
		t.Errorf("flake.nix changed on failure")
	}
}

// TestNixMissingSkipsWithTheNote: with no nix on PATH the step runs nothing, leaves
// flake.nix alone, reports Skipped and writes the skip line to the caller. The
// command still exits 0, as the bead's constraint requires.
func TestNixMissingSkipsWithTheNote(t *testing.T) {
	t.Parallel()
	before := fixture(t, "flake.nix.before")
	fsys := &memFS{files: map[string]string{FlakePath: before}}
	fake := newFakeExec(t, fakeExec{hasNix: false})
	var out bytes.Buffer

	got, err := UpdateVendorHash(fsys, fake.exec(), "/repo", &out)
	if err != nil {
		t.Fatalf("UpdateVendorHash: %v", err)
	}
	if !got.Skipped {
		t.Errorf("result = %+v, want Skipped", got)
	}
	if want := "nix not found: flake.nix vendorHash NOT updated\n"; out.String() != want {
		t.Errorf("caller output = %q, want %q", out.String(), want)
	}
	if len(fake.calls) != 0 {
		t.Errorf("ran %d commands with nix absent, want none", len(fake.calls))
	}
	if fsys.files[FlakePath] != before {
		t.Errorf("flake.nix changed with nix absent")
	}
}

// TestNixMissingReadsNothing: with nix absent the step does not read flake.nix at
// all, so a skip cannot fail on a flake.nix it never needed.
func TestNixMissingReadsNothing(t *testing.T) {
	t.Parallel()
	fsys := &countingFS{memFS: memFS{files: map[string]string{FlakePath: fixture(t, "flake.nix.before")}}}
	fake := newFakeExec(t, fakeExec{hasNix: false})

	if _, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard); err != nil {
		t.Fatalf("UpdateVendorHash: %v", err)
	}
	if fsys.reads != 0 || fsys.writes != 0 {
		t.Errorf("skip did %d reads and %d writes, want none", fsys.reads, fsys.writes)
	}
}

// TestMissingFlakeFails: run from a directory with no flake.nix, the command
// refuses with the script's message and runs nothing.
func TestMissingFlakeFails(t *testing.T) {
	t.Parallel()
	fsys := &memFS{files: map[string]string{}}
	fake := newFakeExec(t, fakeExec{hasNix: true, nixOut: newVendorHash})

	_, err := UpdateVendorHash(fsys, fake.exec(), "/repo", io.Discard)
	const want = "Must run from repository root (flake.nix not found)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if len(fake.calls) != 0 {
		t.Errorf("ran %d commands without flake.nix, want none", len(fake.calls))
	}
}

// containsEntry reports whether env holds the exact entry.
func containsEntry(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}
