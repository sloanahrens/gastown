package binaryinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	keysDir        = "/keys"
	keychainPath   = keysDir + "/gastown-signing.keychain-db"
	passwordPath   = keysDir + "/keychain.pass"
	signSrc        = "built/gt"
	signTmp        = "install-dir/.gt.tmp.1"
	signedName     = "gt"
	samplePassword = "p4ss-w0rd-S3CRET"
)

var (
	errReadPassword  = errors.New("password file unreadable")
	errUnlockRefused = errors.New("unlock refused")
	errCodesignExit  = errors.New("codesign exited 1")
	errRestoreCopy   = errors.New("restore copy refused")
	errRestoreChmod  = errors.New("restore chmod refused")
	errNoHome        = errors.New("no home directory")
)

// signSeam is the filesystem, environment, clock and command runner one Sign
// test drives. The files are an in-memory tree, every command is recorded, and
// each failure is set by the test; everything else succeeds.
type signSeam struct {
	files map[string][]byte
	modes map[string]os.FileMode
	env   map[string]string
	home  string
	now   time.Time

	homeErr     error
	readErr     error
	copyErr     error
	chmodErr    error
	unlockErr   error
	codesignErr error
	// codesignOut is what codesign printed before it failed, and partial is the
	// half-written copy it leaves in the temp file when it fails.
	codesignOut string
	partial     []byte

	calls    []signCall
	deadline time.Time
	warnings []string
	notes    []string
}

type signCall struct {
	name string
	args []string
}

func newSignSeam() *signSeam {
	f := &signSeam{
		files: map[string][]byte{signSrc: []byte("built bytes"), signTmp: []byte("built bytes")},
		modes: map[string]os.FileMode{signSrc: 0o750, signTmp: 0o600},
		env:   map[string]string{"INSTALL_GT_SIGN_DIR": keysDir},
		home:  "/home/tester",
		now:   time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	f.addMaterial(keysDir)
	return f
}

// addMaterial puts a keychain and its password file in dir.
func (f *signSeam) addMaterial(dir string) {
	f.files[dir+"/gastown-signing.keychain-db"] = []byte("keychain")
	f.files[dir+"/keychain.pass"] = []byte(samplePassword + "\n")
}

// signer is the production Signer wired to this seam.
func (f *signSeam) signer() Signer {
	return Signer{
		Installer: Installer{
			Files: Files{
				IsFile: func(path string) bool {
					_, ok := f.files[path]
					return ok
				},
				Mode: func(path string) (os.FileMode, error) {
					mode, ok := f.modes[path]
					if !ok {
						return 0, os.ErrNotExist
					}
					return mode, nil
				},
				Copy: f.copyBytes,
				Chmod: func(path string, perm os.FileMode) error {
					if f.chmodErr != nil {
						return f.chmodErr
					}
					f.modes[path] = perm
					return nil
				},
			},
			Warnf: func(format string, args ...any) {
				f.warnings = append(f.warnings, fmt.Sprintf(format, args...))
			},
		},
		Exec: f.exec,
		ReadFile: func(path string) ([]byte, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			data, ok := f.files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return data, nil
		},
		Getenv: func(key string) string { return f.env[key] },
		Home:   func() (string, error) { return f.home, f.homeErr },
		Now:    func() time.Time { return f.now },
		Notef: func(format string, args ...any) {
			f.notes = append(f.notes, fmt.Sprintf(format, args...))
		},
	}
}

func (f *signSeam) copyBytes(dst, src string) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	data, ok := f.files[src]
	if !ok {
		return os.ErrNotExist
	}
	f.files[dst] = append([]byte(nil), data...)
	return nil
}

// exec answers the two commands Sign runs. codesign writes its signed bytes to
// the temp file, the last argument, or its partial copy when it fails.
func (f *signSeam) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, signCall{name: name, args: args})
	switch name {
	case "security":
		return nil, f.unlockErr
	case "codesign":
		f.deadline, _ = ctx.Deadline()
		tmp := args[len(args)-1]
		if f.codesignErr != nil {
			f.files[tmp] = f.partial
			return []byte(f.codesignOut), f.codesignErr
		}
		f.files[tmp] = []byte("signed bytes")
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %q", name)
}

func (f *signSeam) commandsNamed(name string) []signCall {
	var out []signCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func argsEqual(got, want []string) bool {
	return strings.Join(got, "\x00") == strings.Join(want, "\x00")
}

func TestSignFallsBackToAdHocOnEachMissingPieceWithTheReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*signSeam)
		want  string
	}{
		{
			name:  "missing keychain",
			setup: func(f *signSeam) { delete(f.files, keychainPath) },
			want:  "installing ad-hoc: no signing keychain at " + keychainPath,
		},
		{
			name:  "missing password file",
			setup: func(f *signSeam) { delete(f.files, passwordPath) },
			want:  "installing ad-hoc: no password file at " + passwordPath,
		},
		{
			name:  "unreadable password file",
			setup: func(f *signSeam) { f.readErr = errReadPassword },
			want:  "installing ad-hoc: cannot read " + passwordPath,
		},
		{
			name:  "failed unlock",
			setup: func(f *signSeam) { f.unlockErr = errUnlockRefused },
			want:  "installing ad-hoc: cannot unlock " + keychainPath,
		},
		{
			name: "no town and no home",
			setup: func(f *signSeam) {
				delete(f.env, "INSTALL_GT_SIGN_DIR")
				f.homeErr = errNoHome
			},
			want: "installing ad-hoc: no signing material directory (no town found; set INSTALL_GT_SIGN_DIR)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignSeam()
			tc.setup(f)
			if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
				t.Fatalf("Sign() = %v, want nil: the install proceeds ad-hoc", err)
			}
			if len(f.warnings) != 1 || f.warnings[0] != tc.want {
				t.Errorf("warnings = %q, want [%q]", f.warnings, tc.want)
			}
			if n := len(f.commandsNamed("codesign")); n != 0 {
				t.Errorf("codesign ran %d times, want 0", n)
			}
		})
	}
}

func TestSignFailureRestoresTheTempFromSource(t *testing.T) {
	t.Parallel()
	f := newSignSeam()
	f.codesignErr = errCodesignExit
	f.codesignOut = "errSecInternalComponent\nsecond line"
	f.partial = []byte("half-written")

	if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
		t.Fatalf("Sign() = %v, want nil: a failed codesign installs ad-hoc", err)
	}
	if got := string(f.files[signTmp]); got != "built bytes" {
		t.Errorf("temp after failed codesign = %q, want the source's bytes %q", got, "built bytes")
	}
	if got := f.modes[signTmp]; got != 0o750 {
		t.Errorf("temp mode after failed codesign = %#o, want the source's %#o", got, 0o750)
	}
	want := "installing ad-hoc: codesign failed: errSecInternalComponent"
	if len(f.warnings) != 1 || f.warnings[0] != want {
		t.Errorf("warnings = %q, want [%q]", f.warnings, want)
	}
}

func TestSignNamesACodesignFailureWithNoOutput(t *testing.T) {
	t.Parallel()
	f := newSignSeam()
	f.codesignErr = errCodesignExit

	if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
		t.Fatalf("Sign() = %v, want nil", err)
	}
	want := "installing ad-hoc: codesign failed: no output"
	if len(f.warnings) != 1 || f.warnings[0] != want {
		t.Errorf("warnings = %q, want [%q]", f.warnings, want)
	}
}

func TestSignReturnsTheRestoreError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*signSeam)
		want  error
	}{
		{name: "copy refused", setup: func(f *signSeam) { f.copyErr = errRestoreCopy }, want: errRestoreCopy},
		{name: "chmod refused", setup: func(f *signSeam) { f.chmodErr = errRestoreChmod }, want: errRestoreChmod},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignSeam()
			f.codesignErr = errCodesignExit
			f.partial = []byte("half-written")
			tc.setup(f)

			err := f.signer().Sign(signTmp, signSrc, signedName)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Sign() = %v, want the restore error: the temp holds half-written bytes and must not install", err)
			}
		})
	}
}

func TestSignTimeoutUsesTheConfiguredLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{name: "unset takes thirty seconds", timeout: "", want: 30 * time.Second},
		{name: "whole seconds", timeout: "7", want: 7 * time.Second},
		{name: "fractional seconds", timeout: "2.5", want: 2500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignSeam()
			if tc.timeout != "" {
				f.env["INSTALL_GT_SIGN_TIMEOUT"] = tc.timeout
			}
			if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
				t.Fatalf("Sign() = %v, want nil", err)
			}
			if got := f.deadline.Sub(f.now); got != tc.want {
				t.Errorf("codesign deadline is %v from now, want %v", got, tc.want)
			}
		})
	}
}

func TestSignRefusesATimeoutThatIsNotSeconds(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"soon", "0", "-3", "NaN", "Inf"} {
		t.Run(raw, func(t *testing.T) {
			f := newSignSeam()
			f.env["INSTALL_GT_SIGN_TIMEOUT"] = raw
			if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
				t.Fatalf("Sign() = %v, want nil: a bad timeout installs ad-hoc", err)
			}
			want := fmt.Sprintf("installing ad-hoc: INSTALL_GT_SIGN_TIMEOUT=%q is not a positive number of seconds", raw)
			if len(f.warnings) != 1 || f.warnings[0] != want {
				t.Errorf("warnings = %q, want [%q]", f.warnings, want)
			}
			if len(f.calls) != 0 {
				t.Errorf("%d commands ran, want none: the timeout is judged before any is run", len(f.calls))
			}
		})
	}
}

func TestSignPassesAPathThatStartsWithADashAsAPath(t *testing.T) {
	t.Parallel()
	f := newSignSeam()
	f.env["INSTALL_GT_SIGN_DIR"] = "-keys"
	f.addMaterial("-keys")
	dashTmp := "-install/.gt.tmp.1"

	if err := f.signer().Sign(dashTmp, signSrc, signedName); err != nil {
		t.Fatalf("Sign() = %v, want nil", err)
	}
	unlock := f.commandsNamed("security")
	if len(unlock) != 1 || unlock[0].args[3] != "./-keys/gastown-signing.keychain-db" {
		t.Errorf("unlock call = %q, want its keychain as ./-keys/gastown-signing.keychain-db", unlock)
	}
	codesign := f.commandsNamed("codesign")
	if len(codesign) != 1 {
		t.Fatalf("codesign ran %d times, want 1", len(codesign))
	}
	if got := codesign[0].args[len(codesign[0].args)-1]; got != "./-install/.gt.tmp.1" {
		t.Errorf("codesign temp argument = %q, want ./-install/.gt.tmp.1 so no option can read it", got)
	}
}

func TestSignDisabledByEnv(t *testing.T) {
	t.Parallel()
	f := newSignSeam()
	f.env["INSTALL_BINARY_SIGN"] = "0"

	if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
		t.Fatalf("Sign() = %v, want nil", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("%d commands ran, want none when INSTALL_BINARY_SIGN=0", len(f.calls))
	}
	if len(f.warnings) != 0 || len(f.notes) != 0 {
		t.Errorf("warnings = %q, notes = %q, want none", f.warnings, f.notes)
	}
	if got := string(f.files[signTmp]); got != "built bytes" {
		t.Errorf("temp = %q, want the unsigned copy untouched", got)
	}
}

func TestSignSignsTheTempCopy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		identity string
		want     string
	}{
		{name: "default identity", identity: "", want: "gastown-dev-unattended"},
		{name: "identity from the environment", identity: "team-id", want: "team-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignSeam()
			if tc.identity != "" {
				f.env["INSTALL_GT_SIGN_IDENTITY"] = tc.identity
			}
			if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
				t.Fatalf("Sign() = %v, want nil", err)
			}
			if got := string(f.files[signTmp]); got != "signed bytes" {
				t.Errorf("temp = %q, want the signed copy", got)
			}
			if len(f.warnings) != 0 {
				t.Errorf("warnings = %q, want none on success", f.warnings)
			}
			if want := []string{"signed gt as com.gastown.gt"}; !argsEqual(f.notes, want) {
				t.Errorf("notes = %q, want %q", f.notes, want)
			}
			codesign := f.commandsNamed("codesign")
			if len(codesign) != 1 {
				t.Fatalf("codesign ran %d times, want 1", len(codesign))
			}
			want := []string{"--force", "--keychain", keychainPath, "-s", tc.want, "-i", "com.gastown.gt", signTmp}
			if !argsEqual(codesign[0].args, want) {
				t.Errorf("codesign args = %q, want %q", codesign[0].args, want)
			}
		})
	}
}

func TestSignResolvesTheSigningDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		dir  string
	}{
		{name: "INSTALL_GT_SIGN_DIR", env: map[string]string{"INSTALL_GT_SIGN_DIR": "/elsewhere"}, dir: "/elsewhere"},
		{name: "GT_TOWN_ROOT", env: map[string]string{"GT_TOWN_ROOT": "/town"}, dir: "/town/.runtime/signing"},
		{name: "home fallback", env: map[string]string{}, dir: "/home/tester/gt/.runtime/signing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignSeam()
			delete(f.files, keychainPath)
			delete(f.files, passwordPath)
			f.addMaterial(tc.dir)
			f.env = tc.env

			if err := f.signer().Sign(signTmp, signSrc, signedName); err != nil {
				t.Fatalf("Sign() = %v, want nil", err)
			}
			unlock := f.commandsNamed("security")
			if len(unlock) != 1 {
				t.Fatalf("unlock ran %d times, want 1", len(unlock))
			}
			if got, want := unlock[0].args[3], tc.dir+"/gastown-signing.keychain-db"; got != want {
				t.Errorf("unlocked keychain %q, want %q", got, want)
			}
		})
	}
}

func TestSignNeverPrintsThePassword(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*signSeam)
	}{
		{name: "success", setup: func(*signSeam) {}},
		{name: "failed unlock", setup: func(f *signSeam) { f.unlockErr = errUnlockRefused }},
		{name: "failed codesign", setup: func(f *signSeam) {
			f.codesignErr = errCodesignExit
			f.codesignOut = "boom"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignSeam()
			tc.setup(f)
			err := f.signer().Sign(signTmp, signSrc, signedName)

			printed := strings.Join(append(append([]string{}, f.warnings...), f.notes...), "\n")
			if err != nil {
				printed += "\n" + err.Error()
			}
			if strings.Contains(printed, samplePassword) {
				t.Errorf("password appears in output:\n%s", printed)
			}
			for _, c := range f.commandsNamed("codesign") {
				for _, arg := range c.args {
					if strings.Contains(arg, samplePassword) {
						t.Errorf("password passed to codesign: %q", c.args)
					}
				}
			}
			// The unlock call is the one place the password goes, as today.
			unlock := f.commandsNamed("security")
			if len(unlock) != 1 || !argsEqual(unlock[0].args, []string{"unlock-keychain", "-p", samplePassword, keychainPath}) {
				t.Errorf("unlock call = %q, want the password in its argv only", unlock)
			}
		})
	}
}
