package binaryinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/signing"
)

// signIdentifier is the designated-requirement identifier the installed binary
// is signed under. A Developer Tools grant is keyed on it, not on the binary's
// hash, so one stable identity survives every install path (gt-ykots).
const signIdentifier = "com.gastown.gt"

// The script's defaults for the signing settings the environment does not set.
const (
	defaultSignIdentity = "gastown-dev-unattended"
	defaultSignTimeout  = 30 * time.Second
	// maxSignSeconds is the largest timeout a time.Duration holds, so a
	// configured value cannot overflow into a negative deadline.
	maxSignSeconds = float64(math.MaxInt64 / int64(time.Second))
)

// ErrSigningFailed is the error of a signing step that failed once the signing
// material was found. The install must not go on unsigned then: a dev box with
// no material installs ad-hoc, but a broken signing setup is a failed install,
// so the copy never replaces the installed binary (gt-tpg0p.68). Run maps it to
// exit status 3.
var ErrSigningFailed = errors.New("signing failed")

// signingError is ErrSigningFailed with the step that failed. Its message names
// the step and never the output of a command, which can hold a secret.
type signingError struct{ reason string }

func (e signingError) Error() string { return ErrSigningFailed.Error() + ": " + e.reason }

func (e signingError) Unwrap() error { return ErrSigningFailed }

// Signer signs a copy of a binary with the town's identity before it replaces
// the installed one (gt-ykots). None of the signing material is required: when
// neither the keychain nor its password file is there, the copy is left unsigned
// and installs ad-hoc exactly as it would with no signing at all, and the notice
// says why. Once either file is there, the material must be complete, both
// regular files, or the install fails (ErrSigningFailed), so a signing that is
// set up but broken never installs an unsigned binary over a signed one.
type Signer struct {
	// Installer supplies the Files seam Sign restores through, and the Warnf
	// that receives every notice the signing steps do not fail for.
	Installer Installer
	// Exec runs a command and returns its combined output and its error. nil
	// runs it with os/exec, with no stdin, so the command reads /dev/null.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
	// StdinExec runs the keychain unlock, which takes the password on stdin and
	// never in argv. nil runs it with os/exec and feeds stdin to the command.
	StdinExec signing.StdinExec
	// Lstat reports what is at a material path without following a symlink, so
	// a symlink is found as itself and not as its target; nil is os.Lstat.
	Lstat func(path string) (os.FileInfo, error)
	// ReadFile reads the password file; nil is os.ReadFile.
	ReadFile func(path string) ([]byte, error)
	// Getenv reads the INSTALL_* settings and GT_TOWN_ROOT; nil is os.Getenv.
	Getenv func(key string) string
	// Home is the user's home directory, the town root's fallback when
	// GT_TOWN_ROOT is unset; nil is os.UserHomeDir.
	Home func() (string, error)
	// Now is the clock the codesign timeout is measured from; nil is time.Now.
	Now func() time.Time
	// Notef receives the success line; nil writes it to standard error.
	Notef func(format string, args ...any)
}

// Sign signs tmp, the copy of src that is about to replace the installed binary
// name, with the town's identity. tmp is the file the replace will rename into
// place, so it is signed in place and never the installed binary itself: a
// codesign that dies mid-write leaves its half-written bytes in tmp, and Sign
// restores tmp from src before it returns, so the caller installs either the
// signed copy or the unsigned one.
//
// With neither the keychain nor its password file present the step is skipped:
// one notice and a nil return, which installs ad-hoc. With either one present,
// the material must be two regular files, and every failing step, a missing or
// non-regular file included, returns ErrSigningFailed with the reason, and tmp
// holds the source's bytes or nothing is installed. A restore that failed is
// returned as it is, not as a signing failure, and tmp is then neither the
// signed copy nor the source. The caller must not install it either way.
// INSTALL_BINARY_SIGN=0 skips the step entirely, with no command run.
func (s Signer) Sign(tmp, src, name string) error {
	if s.getenv("INSTALL_BINARY_SIGN") == "0" {
		return nil
	}
	dir, ok := s.signDir()
	if !ok {
		s.adHoc("no signing material directory (no town found; set INSTALL_GT_SIGN_DIR)")
		return nil
	}
	keychain := filepath.Join(dir, "gastown-signing.keychain-db")
	passwordFile := filepath.Join(dir, "keychain.pass")
	identity := s.getenv("INSTALL_GT_SIGN_IDENTITY")
	if identity == "" {
		identity = defaultSignIdentity
	}
	keyInfo, hasKeychain, err := s.material(keychain)
	if err != nil {
		return signingError{"cannot inspect " + keychain + ": " + err.Error()}
	}
	passInfo, hasPassword, err := s.material(passwordFile)
	if err != nil {
		return signingError{"cannot inspect " + passwordFile + ": " + err.Error()}
	}
	if !hasKeychain && !hasPassword {
		s.adHoc("no signing keychain at " + keychain)
		return nil
	}
	if !hasKeychain {
		return signingError{"no signing keychain at " + keychain + ", but the password file " + passwordFile + " is present"}
	}
	if !hasPassword {
		return signingError{"no password file at " + passwordFile + ", but the keychain " + keychain + " is present"}
	}
	if !keyInfo.Mode().IsRegular() {
		return signingError{keychain + " is not a regular file"}
	}
	if !passInfo.Mode().IsRegular() {
		return signingError{passwordFile + " is not a regular file"}
	}
	limit, err := s.timeout()
	if err != nil {
		return signingError{err.Error()}
	}
	data, err := s.readFile(passwordFile)
	if err != nil {
		return signingError{"cannot read " + passwordFile}
	}
	password := strings.TrimRight(string(data), "\n")
	if password == "" {
		return signingError{"password file " + passwordFile + " is empty"}
	}
	// The unlock runs under the same limit as codesign, so a hung unlock fails
	// the install (gt-e2prc). Each step gets its own deadline.
	unlockCtx, cancelUnlock := signing.Bound(context.Background(), s.now, limit)
	err = signing.Unlock(unlockCtx, s.stdinExec(), keychain, password)
	cancelUnlock()
	if err != nil {
		return signingError{"cannot unlock " + keychain}
	}
	ctx, cancel := signing.Bound(context.Background(), s.now, limit)
	defer cancel()
	err = signing.Codesign(ctx, s.exec(), keychain, identity, signIdentifier, tmp, func() error {
		return s.restore(tmp, src)
	})
	var codesignErr signing.CodesignError
	if errors.As(err, &codesignErr) {
		// The notice is the first line of codesign's output, not part of the
		// error: the error names the step only. codesign does not print the
		// keychain password, and the unlock's own output was discarded above,
		// so this one line holds no secret.
		s.Installer.warnf("codesign failed: %s", codesignErr.Notice)
		return signingError{"codesign failed"}
	}
	if err != nil {
		return err
	}
	s.notef("signed %s as %s", name, signIdentifier)
	return nil
}

// signDir is the directory holding the keychain and its password file:
// INSTALL_GT_SIGN_DIR, else the town's .runtime/signing, the town being
// GT_TOWN_ROOT or ~/gt. It reports false when there is no such directory to
// name, and an empty directory must not become a relative one that reads the
// keychain out of the working directory.
func (s Signer) signDir() (string, bool) {
	if dir := s.getenv("INSTALL_GT_SIGN_DIR"); dir != "" {
		return dir, true
	}
	town := s.getenv("GT_TOWN_ROOT")
	if town == "" {
		home, err := s.home()
		if err != nil || home == "" {
			return "", false
		}
		town = filepath.Join(home, "gt")
	}
	return filepath.Join(town, ".runtime", "signing"), true
}

// timeout is the codesign and unlock limit in seconds from
// INSTALL_GT_SIGN_TIMEOUT, else 30. A value that is not a positive number of
// seconds is an error, not a silent default: the error names it and the install
// fails. So is one too short to hold a nanosecond: it would round to a zero
// duration, a deadline already past (gt-e2prc).
func (s Signer) timeout() (time.Duration, error) {
	raw := s.getenv("INSTALL_GT_SIGN_TIMEOUT")
	if raw == "" {
		return defaultSignTimeout, nil
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || !(secs > 0) || secs > maxSignSeconds {
		return 0, fmt.Errorf("INSTALL_GT_SIGN_TIMEOUT=%q is not a positive number of seconds", raw)
	}
	limit := time.Duration(secs * float64(time.Second))
	if limit <= 0 {
		return 0, fmt.Errorf("INSTALL_GT_SIGN_TIMEOUT=%q is below the 1ns the timeout can measure", raw)
	}
	return limit, nil
}

// restore puts tmp back to src's bytes and mode, the state a failed codesign
// can leave it out of. The mode falls back to go build's 0755 when src's cannot
// be read, as the replace does.
func (s Signer) restore(tmp, src string) error {
	files := s.Installer.Files
	if err := files.copy(tmp, src); err != nil {
		return fmt.Errorf("binaryinstall: restore %s from %s after codesign failed: %w", tmp, src, err)
	}
	perm := os.FileMode(0o755)
	if mode, modeErr := files.mode(src); modeErr == nil {
		perm = mode
	}
	if err := files.chmod(tmp, perm); err != nil {
		return fmt.Errorf("binaryinstall: set mode %#o on %s: %w", perm, tmp, err)
	}
	return nil
}

// adHoc is the notice for a signing step that fell back to an unsigned install.
func (s Signer) adHoc(reason string) {
	s.Installer.warnf("installing ad-hoc: %s", reason)
}

func (s Signer) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.Exec != nil {
		return s.Exec(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// exec is run as the shared signing.Exec. Output is combined here, so it all
// comes back as standard output.
func (s Signer) exec() signing.Exec {
	return func(ctx context.Context, name string, args ...string) (string, string, error) {
		out, err := s.run(ctx, name, args...)
		return string(out), "", err
	}
}

// stdinExec is the unlock's runner: StdinExec when it is set, else runStdin.
func (s Signer) stdinExec() signing.StdinExec {
	if s.StdinExec != nil {
		return s.StdinExec
	}
	return runStdin
}

// runStdin runs name with stdin on its standard input, and returns what it
// wrote. Its error names the command, its arguments (the keychain path) and the
// exit status. It never carries stdin or the output, which hold the password.
func runStdin(ctx context.Context, stdin, name string, args ...string) (string, string, error) {
	return runStdinWith(ctx, func(cmd *exec.Cmd) error { return cmd.Run() }, stdin, name, args...)
}

// runStdinWith is runStdin with its run step injected, so a test drives the
// failure path without starting a process.
func runStdinWith(ctx context.Context, run func(*exec.Cmd) error, stdin, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := run(cmd); err != nil {
		return out.String(), errOut.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), errOut.String(), nil
}

// material reports whether anything is at path and what it is, without
// following a symlink. A failure other than a missing entry is an error, never
// absence, so material that cannot be inspected does not count as none.
func (s Signer) material(path string) (os.FileInfo, bool, error) {
	info, err := s.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return info, true, nil
}

func (s Signer) lstat(path string) (os.FileInfo, error) {
	if s.Lstat != nil {
		return s.Lstat(path)
	}
	return os.Lstat(path)
}

func (s Signer) readFile(path string) ([]byte, error) {
	if s.ReadFile != nil {
		return s.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (s Signer) getenv(key string) string {
	if s.Getenv != nil {
		return s.Getenv(key)
	}
	return os.Getenv(key)
}

func (s Signer) home() (string, error) {
	if s.Home != nil {
		return s.Home()
	}
	return os.UserHomeDir()
}

func (s Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Signer) notef(format string, args ...any) {
	if s.Notef != nil {
		s.Notef(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
