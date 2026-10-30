package ominstaller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	signDir      = "/town/.runtime/signing"
	keychainFile = signDir + "/gastown-signing.keychain-db"
	passwordFile = signDir + "/keychain.pass"
	omBuild      = "/home/op/go/bin/.om.new.1"
	password     = "p4ss-w0rd-S3CRET"
	unsignedOM   = "unsigned om build"
)

// fakeRun is an Exec seam that records each command and answers the two commands
// sign runs. A test sets the failure it wants, and onCodesign runs when codesign
// is called, so a test can leave a half-written build behind.
type fakeRun struct {
	calls []Command
	// deadline reports whether codesign's context carried a deadline.
	deadline bool
	// unlockDeadline reports whether the unlock's context carried a deadline.
	unlockDeadline bool

	unlockErr error
	// unlockHangs makes the unlock wait for its context to end, as a hung unlock
	// does. With no deadline on that context it fails at once, so a test of an
	// unbounded unlock goes red rather than waiting forever.
	unlockHangs    bool
	codesignErr    error
	codesignStderr string
	onCodesign     func()
}

// errNoDeadline is what a hung unlock returns when nothing bounds it.
var errNoDeadline = errors.New("unlock ran with no deadline")

func (r *fakeRun) exec(ctx context.Context, c Command) (string, string, error) {
	r.calls = append(r.calls, c)
	switch c.Name {
	case "security":
		_, r.unlockDeadline = ctx.Deadline()
		if r.unlockHangs {
			if !r.unlockDeadline {
				return "", "", errNoDeadline
			}
			<-ctx.Done()
			return "", "", ctx.Err()
		}
		return "", "", r.unlockErr
	case "codesign":
		_, r.deadline = ctx.Deadline()
		if r.onCodesign != nil {
			r.onCodesign()
		}
		return "", r.codesignStderr, r.codesignErr
	}
	return "", "", fmt.Errorf("unexpected command %s", c.Name)
}

// signRig is one sign run's seams: the files with the signing material present
// and the unsigned build at omBuild, the command runner and the log.
type signRig struct {
	files *fakeFiles
	run   *fakeRun
	logs  *logRecorder
	cfg   Config
}

func newSignRig() *signRig {
	files := newFakeFiles()
	files.files[keychainFile] = []byte("keychain")
	files.files[passwordFile] = []byte(password + "\n")
	files.files[omBuild] = []byte(unsignedOM)
	files.modes[omBuild] = 0o755
	run := &fakeRun{}
	logs := &logRecorder{}
	return &signRig{
		files: files,
		run:   run,
		logs:  logs,
		cfg: Config{
			SignDir:      signDir,
			SignIdentity: "gastown-dev-unattended",
			SignWait:     30 * time.Second,
			Exec:         run.exec,
			Files:        files,
			Logf:         logs.logf,
		},
	}
}

func TestSignUsesTheTownIdentity(t *testing.T) {
	t.Parallel()
	rig := newSignRig()
	rig.cfg.SignIdentity = "town-identity"

	if err := sign(context.Background(), rig.cfg, omBuild); err != nil {
		t.Fatalf("sign = %v, want nil", err)
	}
	want := []Command{
		{Name: "security", Args: []string{"unlock-keychain", keychainFile}, Stdin: password + "\n"},
		{Name: "codesign", Args: []string{"--force", "--keychain", keychainFile, "-s", "town-identity", "-i", "com.gastown.om", omBuild}},
	}
	if !reflect.DeepEqual(rig.run.calls, want) {
		t.Errorf("commands = %+v, want %+v", rig.run.calls, want)
	}
	if !rig.run.unlockDeadline {
		t.Error("unlock ran with no deadline, want the sign timeout to bound it")
	}
	if !rig.run.deadline {
		t.Error("codesign ran with no deadline, want the sign timeout to bound it")
	}
	if got, want := rig.logs.lines, []string{"[install-om] Signed the build as com.gastown.om"}; !reflect.DeepEqual(got, want) {
		t.Errorf("log = %q, want %q", got, want)
	}
}

// TestSignKeepsADashLeadingSignDirFromReadingAsAnOption: a relative sign dir that
// starts with a dash would make security and codesign read the keychain path as
// an option, so the keychain is passed from the current directory.
func TestSignKeepsADashLeadingSignDirFromReadingAsAnOption(t *testing.T) {
	t.Parallel()
	const dir = "-signing"
	keychain := dir + "/gastown-signing.keychain-db"
	rig := newSignRig()
	rig.cfg.SignDir = dir
	rig.files.files[keychain] = []byte("keychain")
	rig.files.files[dir+"/keychain.pass"] = []byte(password + "\n")

	if err := sign(context.Background(), rig.cfg, omBuild); err != nil {
		t.Fatalf("sign = %v, want nil", err)
	}
	want := []Command{
		{Name: "security", Args: []string{"unlock-keychain", "./" + keychain}, Stdin: password + "\n"},
		{Name: "codesign", Args: []string{"--force", "--keychain", "./" + keychain, "-s", "gastown-dev-unattended", "-i", "com.gastown.om", omBuild}},
	}
	if !reflect.DeepEqual(rig.run.calls, want) {
		t.Errorf("commands = %+v, want %+v", rig.run.calls, want)
	}
}

func TestSignFallsBackAdHocWithOneWarning(t *testing.T) {
	t.Parallel()
	rig := newSignRig()
	delete(rig.files.files, keychainFile)
	delete(rig.files.files, passwordFile)

	if err := sign(context.Background(), rig.cfg, omBuild); err != nil {
		t.Fatalf("sign = %v, want nil so the build goes in ad-hoc", err)
	}
	if len(rig.run.calls) != 0 {
		t.Errorf("ran %d commands with no signing material, want none", len(rig.run.calls))
	}
	want := []string{"[install-om] WARNING: installing ad-hoc: no keychain or password file in " + signDir}
	if !reflect.DeepEqual(rig.logs.lines, want) {
		t.Errorf("log = %q, want exactly one WARNING line %q", rig.logs.lines, want)
	}
	if got := string(rig.files.files[omBuild]); got != unsignedOM {
		t.Errorf("build = %q, want it left unsigned", got)
	}
}

func TestSignPartialMaterialFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(rig *signRig)
	}{
		{
			name:  "keychain missing beside its password file",
			setup: func(rig *signRig) { delete(rig.files.files, keychainFile) },
		},
		{
			name:  "password file missing beside its keychain",
			setup: func(rig *signRig) { delete(rig.files.files, passwordFile) },
		},
		{
			name: "keychain is not a regular file",
			setup: func(rig *signRig) {
				rig.files.modes[keychainFile] = fs.ModeDir | 0o755
			},
		},
		{
			name: "password file is not a regular file",
			setup: func(rig *signRig) {
				rig.files.modes[passwordFile] = fs.ModeDir | 0o755
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignRig()
			tc.setup(rig)

			err := sign(context.Background(), rig.cfg, omBuild)
			if !errors.Is(err, errSigning) {
				t.Fatalf("sign = %v, want errSigning: half the material is a broken setup, not ad-hoc", err)
			}
			if n := rig.logs.warnings(); n != 0 {
				t.Errorf("WARNING lines = %d, want none", n)
			}
			if len(rig.run.calls) != 0 {
				t.Errorf("ran %d commands with broken signing material, want none", len(rig.run.calls))
			}
			if got := string(rig.files.files[omBuild]); got != unsignedOM {
				t.Errorf("build = %q, want it left unsigned", got)
			}
		})
	}
}

func TestSignFailureAfterMaterialFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(rig *signRig)
		// checkBuild reports whether the build must be the unsigned one again.
		// A restore that itself failed leaves the build as codesign left it.
		checkBuild bool
	}{
		{
			name:       "keychain status unknown",
			setup:      func(rig *signRig) { rig.files.statErr[keychainFile] = errBoom },
			checkBuild: true,
		},
		{
			name:       "password file status unknown",
			setup:      func(rig *signRig) { rig.files.statErr[passwordFile] = errBoom },
			checkBuild: true,
		},
		{
			name:       "password file unreadable",
			setup:      func(rig *signRig) { rig.files.readErr[passwordFile] = errBoom },
			checkBuild: true,
		},
		{
			name:       "timeout is not positive",
			setup:      func(rig *signRig) { rig.cfg.SignWait = -time.Second },
			checkBuild: true,
		},
		{
			name:       "unlock refused",
			setup:      func(rig *signRig) { rig.run.unlockErr = errBoom },
			checkBuild: true,
		},
		{
			name: "unlock times out",
			setup: func(rig *signRig) {
				rig.run.unlockHangs = true
				rig.cfg.SignWait = time.Nanosecond
			},
			checkBuild: true,
		},
		{
			name:       "password file is empty",
			setup:      func(rig *signRig) { rig.files.files[passwordFile] = []byte("\n") },
			checkBuild: true,
		},
		{
			name: "codesign fails",
			setup: func(rig *signRig) {
				rig.run.codesignErr = errBoom
				rig.run.onCodesign = func() { rig.files.files[omBuild] = []byte("half-wri") }
			},
			checkBuild: true,
		},
		{
			name: "codesign times out",
			setup: func(rig *signRig) {
				rig.run.codesignErr = context.DeadlineExceeded
				rig.run.onCodesign = func() { rig.files.files[omBuild] = []byte("half-wri") }
			},
			checkBuild: true,
		},
		{
			name: "restore cannot write the build back",
			setup: func(rig *signRig) {
				rig.run.codesignErr = errBoom
				rig.run.onCodesign = func() { rig.files.writeErr = errBoom }
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignRig()
			tc.setup(rig)

			err := sign(context.Background(), rig.cfg, omBuild)
			if !errors.Is(err, errSigning) {
				t.Fatalf("sign = %v, want errSigning so the install fails with reason sign", err)
			}
			if n := rig.logs.warnings(); n != 0 {
				t.Errorf("WARNING lines = %d, want none: a broken signing setup is a failure, not ad-hoc", n)
			}
			if tc.checkBuild {
				if got := string(rig.files.files[omBuild]); got != unsignedOM {
					t.Errorf("build = %q, want the unsigned build restored", got)
				}
			}
		})
	}
}

func TestSignUnlockOverBudgetNamesTheTimeout(t *testing.T) {
	t.Parallel()
	rig := newSignRig()
	rig.run.unlockHangs = true
	rig.cfg.SignWait = time.Nanosecond

	err := sign(context.Background(), rig.cfg, omBuild)
	if !errors.Is(err, errSigning) {
		t.Fatalf("sign = %v, want errSigning", err)
	}
	if want := "unlock exceeded 1ns"; !strings.Contains(err.Error(), want) {
		t.Errorf("sign = %q, want the reason to name the timeout: %q", err, want)
	}
	if n := len(rig.run.calls); n != 1 {
		t.Errorf("ran %d commands, want only the unlock: codesign must not run after it timed out", n)
	}
}

func TestSignEmptyPasswordFailsBeforeUnlock(t *testing.T) {
	t.Parallel()
	rig := newSignRig()
	rig.files.files[passwordFile] = []byte("\n")

	err := sign(context.Background(), rig.cfg, omBuild)
	if !errors.Is(err, errSigning) {
		t.Fatalf("sign = %v, want errSigning: an empty password is a broken setup", err)
	}
	if n := len(rig.run.calls); n != 0 {
		t.Errorf("ran %d commands with an empty password, want none: the unlock must not run", n)
	}
}

// TestSignRealFilesystemJudgesMaterialByItsEntry runs sign over the real
// filesystem in a temporary directory. The link rows need a link on disk: a Stat
// that followed links would read a dangling one as absent, and a link to a
// regular file as regular.
func TestSignRealFilesystemJudgesMaterialByItsEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		// wantSigning is whether the material must fail closed with errSigning.
		wantSigning bool
		// wantCommands is how many commands must run: the unlock and codesign
		// when the material is good, none otherwise.
		wantCommands int
		// wantWarnings is how many WARNING lines must be logged.
		wantWarnings int
	}{
		{
			name:         "neither present: ad-hoc with one WARNING",
			setup:        func(t *testing.T, dir string) {},
			wantWarnings: 1,
		},
		{
			name: "dangling link at the keychain",
			setup: func(t *testing.T, dir string) {
				symlink(t, filepath.Join(dir, "missing"), filepath.Join(dir, keychainName))
			},
			wantSigning: true,
		},
		{
			name: "dangling link at the password file",
			setup: func(t *testing.T, dir string) {
				symlink(t, filepath.Join(dir, "missing"), filepath.Join(dir, passwordFileName))
			},
			wantSigning: true,
		},
		{
			name: "link to a regular keychain",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "real-keychain"), "keychain")
				symlink(t, filepath.Join(dir, "real-keychain"), filepath.Join(dir, keychainName))
				writeFile(t, filepath.Join(dir, passwordFileName), password+"\n")
			},
			wantSigning: true,
		},
		{
			name: "link to a regular password file",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, keychainName), "keychain")
				writeFile(t, filepath.Join(dir, "real-password"), password+"\n")
				symlink(t, filepath.Join(dir, "real-password"), filepath.Join(dir, passwordFileName))
			},
			wantSigning: true,
		},
		{
			name: "both regular: signs",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, keychainName), "keychain")
				writeFile(t, filepath.Join(dir, passwordFileName), password+"\n")
			},
			wantCommands: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			build := filepath.Join(dir, "om.new")
			writeFile(t, build, unsignedOM)
			tc.setup(t, dir)

			run := &fakeRun{}
			logs := &logRecorder{}
			cfg := Config{
				SignDir:      dir,
				SignIdentity: "gastown-dev-unattended",
				SignWait:     30 * time.Second,
				Exec:         run.exec,
				Files:        osFiles{},
				Logf:         logs.logf,
			}
			err := sign(context.Background(), cfg, build)
			if tc.wantSigning && !errors.Is(err, errSigning) {
				t.Errorf("sign = %v, want errSigning: broken material must not go ad-hoc", err)
			}
			if !tc.wantSigning && err != nil {
				t.Errorf("sign = %v, want nil", err)
			}
			if n := len(run.calls); n != tc.wantCommands {
				t.Errorf("ran %d commands, want %d", n, tc.wantCommands)
			}
			if n := logs.warnings(); n != tc.wantWarnings {
				t.Errorf("WARNING lines = %d, want %d", n, tc.wantWarnings)
			}
			if got, err := os.ReadFile(build); err != nil || string(got) != unsignedOM {
				t.Errorf("build = %q (%v), want it left unsigned", got, err)
			}
		})
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
