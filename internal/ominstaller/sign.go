package ominstaller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// signIdentifier is the designated-requirement identifier om is signed under. A
// Developer Tools grant is keyed on it, so om keeps its own identity, not gt's.
const signIdentifier = "com.gastown.om"

// The keychain and its password file in the signing directory, the material the
// gt installer signs gt with.
const (
	keychainName     = "gastown-signing.keychain-db"
	passwordFileName = "keychain.pass"
)

// errSigning is the failure of a signing step once the signing material is found.
// A caller reports the install failed with reason sign and installs nothing: a
// broken signing setup must not put an unsigned om over a signed one.
var errSigning = errors.New("signing failed")

// sign signs tmp, the build about to replace om, with the town's identity. With no
// signing material, neither the keychain nor the password file, it logs one
// WARNING and returns nil, and the build goes in ad-hoc. A keychain or password
// file without the other is a broken setup, not an absent one, so it fails. Once
// the material is found, any failing step returns errSigning and tmp holds the
// unsigned build again, so the caller installs nothing.
//
// The unsigned build is read before codesign runs, because codesign rewrites tmp
// in place and a failed run can leave it half-written.
//
// Material is judged by its own entry, a symbolic link counted as an entry, so a
// dangling link is material and fails rather than reading as absent. The unlock
// and codesign each get their own sign timeout, not one shared deadline. install
// calls it on the build before the previous om is kept.
func sign(ctx context.Context, cfg Config, tmp string) error {
	files := cfg.files()
	keychain := filepath.Join(cfg.SignDir, keychainName)
	passwordFile := filepath.Join(cfg.SignDir, passwordFileName)

	keychainMode, haveKeychain, err := lstatOptional(files, keychain)
	if err != nil {
		return signFailed("cannot tell whether " + keychain + " exists")
	}
	passwordMode, havePassword, err := lstatOptional(files, passwordFile)
	if err != nil {
		return signFailed("cannot tell whether " + passwordFile + " exists")
	}
	if !haveKeychain && !havePassword {
		cfg.logf("[install-om] WARNING: installing ad-hoc: no keychain or password file in %s", cfg.SignDir)
		return nil
	}
	if !haveKeychain {
		return signFailed("keychain " + keychain + " is missing beside its password file")
	}
	if !havePassword {
		return signFailed("password file " + passwordFile + " is missing beside its keychain")
	}
	if !keychainMode.IsRegular() {
		return signFailed(keychain + " is not a regular file")
	}
	if !passwordMode.IsRegular() {
		return signFailed(passwordFile + " is not a regular file")
	}

	if cfg.SignWait <= 0 {
		return signFailed("the signing timeout is not positive")
	}
	data, err := files.ReadFile(passwordFile)
	if err != nil {
		return signFailed("cannot read " + passwordFile)
	}
	password := strings.TrimRight(string(data), "\n")
	if password == "" {
		return signFailed("password file " + passwordFile + " is empty")
	}

	if err := unlockKeychain(ctx, cfg, keychain, password); err != nil {
		if errors.Is(err, errUnlockTimeout) {
			return signFailed(fmt.Sprintf("unlock exceeded %s", cfg.SignWait))
		}
		return signFailed("cannot unlock " + keychain)
	}

	unsigned, err := files.ReadFile(tmp)
	if err != nil {
		return signFailed("cannot read the build to sign at " + tmp)
	}
	perm, err := files.Stat(tmp)
	if err != nil {
		return signFailed("cannot read the mode of " + tmp)
	}

	signCtx, cancel := context.WithTimeout(ctx, cfg.SignWait)
	defer cancel()
	stdout, stderr, err := cfg.Exec(signCtx, Command{Name: "codesign", Args: []string{"--force", "--keychain", keychain, "-s", cfg.SignIdentity, "-i", signIdentifier, tmp}})
	if err != nil {
		if restoreErr := restore(files, tmp, unsigned, perm.Perm()); restoreErr != nil {
			return signFailed("cannot restore the unsigned build at " + tmp + " after codesign failed")
		}
		// codesign's first line says what it objected to. It is logged, not put in
		// the error, which names the step only.
		out := stderr
		if out == "" {
			out = stdout
		}
		cfg.logf("[install-om] codesign failed: %s", firstLine(out))
		return signFailed("codesign failed or timed out")
	}
	cfg.logf("[install-om] Signed the build as %s", signIdentifier)
	return nil
}

// errUnlockTimeout is the unlock running past the sign timeout.
var errUnlockTimeout = errors.New("unlock timed out")

// unlockKeychain unlocks the keychain under the sign timeout, so a hung unlock fails
// the install as a hung codesign does. unlock-keychain -p takes the password in its
// argv, so ps can see it while the unlock runs. The unlock's output is discarded,
// so the password is never logged. A run cut off by its own deadline returns
// errUnlockTimeout, so the caller can name the timeout.
func unlockKeychain(ctx context.Context, cfg Config, keychain, password string) error {
	unlockCtx, cancel := context.WithTimeout(ctx, cfg.SignWait)
	defer cancel()
	_, _, err := cfg.Exec(unlockCtx, Command{Name: "security", Args: []string{"unlock-keychain", "-p", password, keychain}})
	if err != nil && unlockCtx.Err() == context.DeadlineExceeded {
		return errUnlockTimeout
	}
	return err
}

// restore puts the unsigned build back at tmp, the state a failed codesign can
// leave it out of.
func restore(files Files, tmp string, unsigned []byte, perm fs.FileMode) error {
	if err := files.WriteFile(tmp, unsigned, perm); err != nil {
		return err
	}
	return files.Chmod(tmp, perm)
}

// signFailed is errSigning with the step that failed. The reason names a path or
// a step, never the output of a command, which can hold a secret.
func signFailed(reason string) error {
	return fmt.Errorf("%w: %s", errSigning, reason)
}

// firstLine is the first line of s, or "no output" when s is empty.
func firstLine(s string) string {
	if s == "" {
		return "no output"
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}
