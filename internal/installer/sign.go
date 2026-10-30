package installer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/signing"
)

// signIdentifier is the designated-requirement identifier the installed binary
// is signed under. A Developer Tools grant is keyed on it, not on the binary's
// hash, which is what makes one stable identity survive every install
// (gt-426fo.1).
const signIdentifier = "com.gastown.gt"

// Signing is the town's signing material and the identity it signs with: a
// keychain in SIGN_DIR, a 0600 file holding its password beside it, and the
// identity's name. None of it is required, and every way it can be missing or
// broken leaves an ad-hoc install exactly as it would have been without any
// signing at all.
type Signing struct {
	// Dir holds the keychain and its password file (INSTALL_GT_SIGN_DIR, else
	// <town>/.runtime/signing).
	Dir string
	// Identity is the codesign -s value (INSTALL_GT_SIGN_IDENTITY, else
	// gastown-dev-unattended).
	Identity string
	// Timeout is the seconds codesign gets where timeout(1) is on PATH
	// (INSTALL_GT_SIGN_TIMEOUT, else 30).
	Timeout string
}

// ResolveSigning reads the signing settings from the environment, taking the
// script's defaults for the ones it does not set. townRoot is empty when the
// run could not find a town, and then Dir stays empty: there is no default
// material directory, and an empty Dir must not become a relative one, which
// would read a keychain and password out of whatever directory the install
// happens to run in (gt-tpg0p.11).
func ResolveSigning(getenv func(string) string, townRoot string) Signing {
	s := Signing{
		Dir:      getenv("INSTALL_GT_SIGN_DIR"),
		Identity: getenv("INSTALL_GT_SIGN_IDENTITY"),
		Timeout:  getenv("INSTALL_GT_SIGN_TIMEOUT"),
	}
	if s.Dir == "" && townRoot != "" {
		s.Dir = filepath.Join(townRoot, ".runtime", "signing")
	}
	if s.Identity == "" {
		s.Identity = "gastown-dev-unattended"
	}
	if s.Timeout == "" {
		s.Timeout = "30"
	}
	return s
}

// Keychain is the keychain the password file unlocks.
func (s Signing) Keychain() string { return filepath.Join(s.Dir, "gastown-signing.keychain-db") }

// PasswordFile is the 0600 file holding the keychain's password.
func (s Signing) PasswordFile() string { return filepath.Join(s.Dir, "keychain.pass") }

// unlockLine is the one command security -i reads from stdin to unlock the
// keychain: the password and the keychain path are words on that line rather
// than in argv, so ps shows neither for the length of the unlock (gt-4ucnp).
//
// It reports false when either word spans a line break. security -i reads a
// line at a time, so a word carrying a newline would become a second command
// rather than part of the first.
//
// security -i's parser is undocumented, so the quoting is what it was observed
// to accept (gt-4ucnp, checked by hand on macOS against a throwaway keychain: a
// word holding a space, a quote and a backslash unlocks, and a wrong password
// fails with status 51): an ordinary word passes as it is and only a word that
// needs quoting is wrapped and escaped.
func unlockLine(password, keychain string) (string, bool) {
	if strings.ContainsAny(password, "\r\n") || strings.ContainsAny(keychain, "\r\n") {
		return "", false
	}
	return "unlock-keychain -p " + quoteLine(password) + " " + quoteLine(keychain) + "\n", true
}

// quoteLine quotes one word of the line security -i reads. A word that is
// non-empty and holds none of a space, a tab, a double quote, an apostrophe or
// a backslash passes as it is; any other word, the empty one included, is
// wrapped in double quotes with its backslashes and double quotes escaped.
func quoteLine(word string) string {
	if word != "" && !strings.ContainsAny(word, " \t\n\"'\\") {
		return word
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(word)
	return `"` + escaped + `"`
}

// Sign is the script's sign_binary over the installed binary: what
// install-local just put in place is signed before the smoke check runs it, so
// the smoke check exercises exactly the artifact the town will execute and
// nothing is signed that has not already installed successfully.
//
// The target is the installed binary, never the build output: `make build`
// stamps a fresh build time into every invocation, so install-local's own build
// prerequisite re-links and a signature on the checkout's binary is gone before
// it is copied.
//
// Every failure here is one WARNING and never a failed install, never a
// rollback: the run proceeds with an ad-hoc binary, which is what a machine
// with no signing material has always installed.
func (r *Run) Sign() {
	bin := r.Paths.GT()
	switch {
	case !r.Files.isFile(bin):
		r.C.Log("WARNING: not signing: nothing installed at " + bin)
		return
	case r.Signing.Dir == "":
		// A run with no town and no INSTALL_GT_SIGN_DIR has no material
		// directory. Reading one from an empty Dir would resolve beside the
		// current directory, so the step installs ad-hoc instead.
		r.C.Log("WARNING: installing ad-hoc: no signing material directory (no town found; set INSTALL_GT_SIGN_DIR)")
		return
	case !r.Files.isFile(r.Signing.Keychain()):
		r.C.Log("WARNING: installing ad-hoc: no keychain at " + r.Signing.Keychain())
		return
	case !r.Files.isFile(r.Signing.PasswordFile()):
		r.C.Log("WARNING: installing ad-hoc: no password file at " + r.Signing.PasswordFile())
		return
	}
	data, err := r.Files.readFile(r.Signing.PasswordFile())
	if err != nil {
		r.C.Log("WARNING: installing ad-hoc: cannot read " + r.Signing.PasswordFile())
		return
	}
	password := strings.TrimRight(string(data), "\n")
	line, ok := unlockLine(password, r.Signing.Keychain())
	if !ok {
		r.C.Log("WARNING: installing ad-hoc: the password or the keychain path spans more than one line")
		return
	}
	// The password reaches security on stdin and nowhere else: it is never in
	// the unlock's argv, where ps shows it for as long as the unlock takes, and
	// never logged, never echoed and never handed to codesign (gt-4ucnp). The
	// shared signing.Unlock now sends the password on stdin too (gt-tpg0p.70), so
	// this unlock keeps its own security -i form: that is the form the macOS
	// integration test pins (gt-4ucnp), and moving onto signing.Unlock is a
	// separate decision.
	//
	// A status of 0 is not taken as proof the keychain opened: what security -i
	// reports for the command it reads is undocumented, and the macOS form is
	// pinned by TestIntegrationSecurityReadsTheUnlockLineFromStdin. The status
	// only says when to stop; codesign is the arbiter, and a keychain that
	// stayed locked fails it below, which installs ad-hoc with one WARNING.
	if _, _, code := r.Cmd.runInput("", "security", []byte(line), "-i"); code != 0 {
		r.C.Log("WARNING: installing ad-hoc: cannot unlock " + r.Signing.Keychain())
		return
	}
	// Sign a temporary copy beside the binary: signing the live path in place
	// would rewrite the bytes of a binary the town may be exec'ing, and a
	// codesign that dies mid-write would leave a half-signed file there. The
	// copy is renamed over the binary only once codesign has succeeded, and it
	// carries the binary's mode, so the rename cannot leave the installed file
	// as the 0600 mktemp created.
	tmp, err := r.Files.tempFile(filepath.Dir(bin), "."+filepath.Base(bin)+".signed.*")
	if err != nil {
		r.C.Log("WARNING: installing ad-hoc: cannot create a temp file beside " + bin)
		return
	}
	if err := r.Files.copy(tmp, bin); err != nil {
		_ = r.Files.remove(tmp)
		r.C.Log("WARNING: installing ad-hoc: cannot copy " + bin)
		return
	}
	r.Files.clearImmutable(tmp)
	// The restore removes the copy, the only file codesign writes, so the
	// installed binary is never touched. A failed removal is not reported: it
	// leaves a stray temp file, never a half-signed binary.
	err = signing.CodesignWith(func() (string, string, error) {
		output, err := r.codesign(tmp)
		return output, "", err
	}, func() error {
		_ = r.Files.remove(tmp)
		return nil
	})
	// Any error stops the sign, so a copy that was never signed is never renamed
	// over the binary, whatever the restore comes to report.
	if err != nil {
		notice := err.Error()
		var codesignErr signing.CodesignError
		if errors.As(err, &codesignErr) {
			notice = codesignErr.Notice
		}
		r.C.Log("WARNING: installing ad-hoc: codesign failed: " + notice)
		return
	}
	r.Files.clearImmutable(bin)
	if err := r.Files.rename(tmp, bin); err != nil {
		_ = r.Files.remove(tmp)
		r.Files.setImmutable(bin)
		r.C.Log("WARNING: installing ad-hoc: cannot rename the signed copy over " + bin)
		return
	}
	r.Files.setImmutable(bin)
	r.C.Log("Signed " + bin + " as " + signIdentifier)
}

// codesign signs path with the town's identity, bounded by the signing timeout
// where timeout(1) (coreutils) or gtimeout (brew's) is on PATH. Where neither
// is, codesign runs unguarded: it gets no stdin and an already-unlocked
// keychain, so it has nothing left to prompt for. The bound stays an argv
// prefix, not a signing.Bound context, so the install keeps the script's guard.
func (r *Run) codesign(path string) (string, error) {
	name := "codesign"
	args := signing.CodesignArgs(r.Signing.Keychain(), r.Signing.Identity, signIdentifier, path)
	switch {
	case r.found("timeout"):
		name = "timeout"
		args = append([]string{r.Signing.Timeout, "codesign"}, args...)
	case r.found("gtimeout"):
		name = "gtimeout"
		args = append([]string{r.Signing.Timeout, "codesign"}, args...)
	}
	stdout, stderr, code := r.Cmd.run("", name, args...)
	output := string(stdout) + string(stderr)
	if code != 0 {
		return output, fmt.Errorf("codesign exited with status %d", code)
	}
	return output, nil
}
