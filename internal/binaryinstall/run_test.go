package binaryinstall

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// runRig is one Run's seams: the replace's recorded filesystem, and a signing
// command runner that logs into the same call trail, so a test reads the order
// the steps ran in. No path here is real: the install directory and the signing
// material are the seam's, and the environment is the rig's map.
type runRig struct {
	*seam
	stderr bytes.Buffer
	env    map[string]string

	codesign    [][]string
	codesignOut []byte
	codesignErr error
	// renamedWhenSigned is the rename target at the moment codesign ran.
	renamedWhenSigned string
}

func newRunRig() *runRig {
	return &runRig{seam: newSeam(), env: map[string]string{"INSTALL_GT_SIGN_DIR": keysDir}}
}

func (r *runRig) run(args ...string) int {
	return Run(args, &r.stderr, Installer{Files: r.files}, Signer{
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			r.log("exec", name+" "+strings.Join(args, " "))
			if name != "codesign" {
				return nil, nil
			}
			r.codesign = append(r.codesign, args)
			r.renamedWhenSigned = r.renamed
			return r.codesignOut, r.codesignErr
		},
		ReadFile: func(string) ([]byte, error) { return []byte(samplePassword + "\n"), nil },
		Getenv:   func(key string) string { return r.env[key] },
	})
}

// callIndex is the position of the first call in the trail that starts with
// prefix, or -1 when no such call was made.
func (r *runRig) callIndex(prefix string) int {
	for i, call := range r.calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

func (r *runRig) countCalls(prefix string) int {
	n := 0
	for _, call := range r.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func TestRunInstallsAndSignsInOrder(t *testing.T) {
	t.Parallel()
	r := newRunRig()
	if code := r.run(testSrc, testDestDir, "gt"); code != 0 {
		t.Fatalf("Run = %d, want 0 (stderr: %s)", code, r.stderr.String())
	}
	copyAt := r.callIndex("copy " + signTmp + " <- " + testSrc)
	signAt := r.callIndex("exec codesign ")
	clearAt := r.callIndex("clear-immutable " + testDest)
	renameAt := r.callIndex("rename " + signTmp + " -> " + testDest)
	if copyAt < 0 || signAt < 0 || clearAt < 0 || renameAt < 0 {
		t.Fatalf("missing a step: copy=%d sign=%d clear=%d rename=%d; calls %v", copyAt, signAt, clearAt, renameAt, r.calls)
	}
	if !(copyAt < signAt && signAt < clearAt && clearAt < renameAt) {
		t.Fatalf("steps ran out of order: copy=%d sign=%d clear=%d rename=%d; calls %v", copyAt, signAt, clearAt, renameAt, r.calls)
	}
	if len(r.codesign) != 1 {
		t.Fatalf("codesign ran %d times, want 1", len(r.codesign))
	}
	want := []string{"--force", "--keychain", keychainPath, "-s", defaultSignIdentity, "-i", signIdentifier, signTmp}
	if !argsEqual(r.codesign[0], want) {
		t.Errorf("codesign args = %q, want %q", r.codesign[0], want)
	}
	if r.stderr.String() != "install-binary: signed gt as com.gastown.gt\n" {
		t.Errorf("stderr = %q, want the one success line", r.stderr.String())
	}
	if strings.Contains(r.stderr.String(), samplePassword) {
		t.Errorf("stderr printed the keychain password: %q", r.stderr.String())
	}
}

func TestRunUsageAndExitCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"no install dir", []string{testSrc}},
		{"empty source", []string{"", testDestDir}},
		{"empty install dir", []string{testSrc, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRunRig()
			if code := r.run(tc.args...); code != 1 {
				t.Fatalf("Run = %d, want 1", code)
			}
			if got := r.stderr.String(); got != runUsage+"\n" {
				t.Errorf("stderr = %q, want the usage line", got)
			}
			if len(r.calls) != 0 {
				t.Errorf("a usage error touched the filesystem: %v", r.calls)
			}
		})
	}

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()
		r := newRunRig()
		r.missing = true
		if code := r.run(testSrc, testDestDir, "gt"); code != 1 {
			t.Fatalf("Run = %d, want 1", code)
		}
		want := "install-binary: source binary not found: " + testSrc + "\n"
		if r.stderr.String() != want {
			t.Errorf("stderr = %q, want %q", r.stderr.String(), want)
		}
		if n := r.countCalls("mkdir-all") + r.countCalls("temp-file") + r.countCalls("copy"); n != 0 {
			t.Errorf("a missing source touched the install dir: %v", r.calls)
		}
	})

	t.Run("a failed replace exits 1 with its error", func(t *testing.T) {
		t.Parallel()
		r := newRunRig()
		r.mkdirErr = errMkdir
		if code := r.run(testSrc, testDestDir, "gt"); code != 1 {
			t.Fatalf("Run = %d, want 1", code)
		}
		if !strings.HasPrefix(r.stderr.String(), "install-binary: binaryinstall: create "+testDestDir) {
			t.Errorf("stderr = %q, want the install-binary prefix and the mkdir error", r.stderr.String())
		}
	})
}

func TestRunDefaultsNameToTheSourceBase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"two arguments take the source base", []string{"built/gt-linux-arm64", testDestDir}, "gt-linux-arm64"},
		{"an empty name takes the source base", []string{"built/gt-linux-arm64", testDestDir, ""}, "gt-linux-arm64"},
		{"a given name wins over the base", []string{"built/gt-linux-arm64", testDestDir, "gt"}, "gt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRunRig()
			if code := r.run(tc.args...); code != 0 {
				t.Fatalf("Run = %d, want 0 (stderr: %s)", code, r.stderr.String())
			}
			if want := testDestDir + "/" + tc.want; r.renamed != want {
				t.Errorf("renamed to %q, want %q", r.renamed, want)
			}
			if want := "install-binary: signed " + tc.want + " as com.gastown.gt\n"; r.stderr.String() != want {
				t.Errorf("stderr = %q, want %q", r.stderr.String(), want)
			}
		})
	}
}

func TestRunFailedSignInstallsTheBuiltBinaryWithOneWarning(t *testing.T) {
	t.Parallel()
	r := newRunRig()
	r.codesignErr = errCodesignExit
	r.codesignOut = []byte("bad signature\nsecond line\n")
	if code := r.run(testSrc, testDestDir, "gt"); code != 0 {
		t.Fatalf("Run = %d, want 0: a failing codesign installs ad-hoc", code)
	}
	if r.renamed != testDest {
		t.Errorf("renamed to %q, want %q", r.renamed, testDest)
	}
	want := "install-binary: installing ad-hoc: codesign failed: bad signature\n"
	if r.stderr.String() != want {
		t.Errorf("stderr = %q, want exactly one warning %q", r.stderr.String(), want)
	}
	// The temp copy was rebuilt from the source after the failed codesign: one
	// copy for the install, one for the restore.
	if n := r.countCalls("copy " + signTmp + " <- " + testSrc); n != 2 {
		t.Errorf("copied the source %d times, want 2 (install and restore)", n)
	}
}

func TestRunDoesNotRenameWhenTheRestoreAfterAFailedSignFails(t *testing.T) {
	t.Parallel()
	r := newRunRig()
	r.codesignErr = errCodesignExit
	copies := 0
	inner := r.files.Copy
	r.files.Copy = func(dst, src string) error {
		copies++
		if copies > 1 {
			return errRestoreCopy
		}
		return inner(dst, src)
	}
	if code := r.run(testSrc, testDestDir, "gt"); code != 1 {
		t.Fatalf("Run = %d, want 1: a restore that failed must not install", code)
	}
	if r.renamed != "" {
		t.Errorf("renamed %q after a failed restore, want no rename", r.renamed)
	}
	if !strings.Contains(r.stderr.String(), "restore") {
		t.Errorf("stderr = %q, want the restore error", r.stderr.String())
	}
}

func TestRunSkipsSigningWhenItIsDisabled(t *testing.T) {
	t.Parallel()
	r := newRunRig()
	r.env["INSTALL_BINARY_SIGN"] = "0"
	if code := r.run(testSrc, testDestDir, "gt"); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if len(r.codesign) != 0 || r.callIndex("exec ") >= 0 {
		t.Errorf("INSTALL_BINARY_SIGN=0 ran a command: %v", r.calls)
	}
	if r.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want none when signing is disabled", r.stderr.String())
	}
	if r.renamed != testDest {
		t.Errorf("renamed to %q, want %q", r.renamed, testDest)
	}
}

func TestRunInstallsAdHocWhenASigningPieceIsMissing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		missing string
		notice  string
	}{
		{"no keychain", keychainPath, "no signing keychain at " + keychainPath},
		{"no password file", passwordPath, "no password file at " + passwordPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRunRig()
			inner := r.files.IsFile
			r.files.IsFile = func(path string) bool { return path != tc.missing && inner(path) }
			if code := r.run(testSrc, testDestDir, "gt"); code != 0 {
				t.Fatalf("Run = %d, want 0: missing signing material installs ad-hoc", code)
			}
			if want := "install-binary: installing ad-hoc: " + tc.notice + "\n"; r.stderr.String() != want {
				t.Errorf("stderr = %q, want %q", r.stderr.String(), want)
			}
			if len(r.codesign) != 0 || r.callIndex("exec security") >= 0 {
				t.Errorf("signing ran without its material: %v", r.calls)
			}
			if r.renamed != testDest {
				t.Errorf("renamed to %q, want %q", r.renamed, testDest)
			}
		})
	}
}

func TestRunLeavesTheImmutableFlagAloneWhenTheCopyFails(t *testing.T) {
	t.Parallel()
	r := newRunRig()
	r.copyErr = errCopy
	r.flags[testDest] = true
	if code := r.run(testSrc, testDestDir, "gt"); code != 1 {
		t.Fatalf("Run = %d, want 1", code)
	}
	if !r.flags[testDest] {
		t.Errorf("a failed copy took the immutable flag off %s: %v", testDest, r.calls)
	}
	if r.callIndex("clear-immutable ") >= 0 || r.callIndex("set-immutable ") >= 0 {
		t.Errorf("a failed copy touched the flag: %v", r.calls)
	}
	if len(r.codesign) != 0 {
		t.Errorf("a failed copy was signed: %v", r.codesign)
	}
}

func TestRunNeverSignsTheLivePath(t *testing.T) {
	t.Parallel()
	r := newRunRig()
	if code := r.run(testSrc, testDestDir, "gt"); code != 0 {
		t.Fatalf("Run = %d, want 0 (stderr: %s)", code, r.stderr.String())
	}
	if r.renamedWhenSigned != "" {
		t.Errorf("the live path was already renamed to %q when codesign ran", r.renamedWhenSigned)
	}
	if len(r.codesign) != 1 || r.codesign[0][len(r.codesign[0])-1] != signTmp {
		t.Errorf("codesign args = %q, want the temp copy %s as the last argument", r.codesign, signTmp)
	}
}
