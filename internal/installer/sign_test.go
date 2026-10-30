package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testPassword is the password file's content, with the newline keychain.pass
// carries.
const testPassword = "test-password\n"

// signedBinary is what the fake codesign writes into the file it signs, so a
// test can tell the renamed copy from the binary it replaced: a copy that
// happened to keep the old bytes would pass unnoticed (gt-tpg0p.11).
const signedBinary = "signed binary"

// signsCopy answers a run the way a working codesign does: the unlock succeeds
// and the file it is handed is rewritten in place, keeping the mode the copy
// was given.
func signsCopy(dir, name string, args []string) ([]byte, []byte, int) {
	if name == "security" {
		return nil, nil, 0
	}
	path := args[len(args)-1]
	info, err := os.Stat(path)
	if err != nil {
		return []byte(err.Error()), nil, 1
	}
	if err := os.WriteFile(path, []byte(signedBinary), info.Mode().Perm()); err != nil {
		return []byte(err.Error()), nil, 1
	}
	return nil, nil, 0
}

// signWorld is a run with a binary installed and the town's signing material
// beside it: a keychain and the 0600 password file that unlocks it. A case
// removes or breaks the part it is about.
type signWorld struct {
	*testRun
	bin       string
	installed string
	keychain  string
	cleared   []string
	set       []string
}

func newSignWorld(t *testing.T) *signWorld {
	t.Helper()
	tr := newTestRun(t)
	// installed stays at the binary the case started with while bin follows a
	// case that moves the install directory, so a failure can be checked against
	// the file the step was handed (gt-tpg0p.11).
	w := &signWorld{testRun: tr, bin: tr.Paths.GT(), installed: tr.Paths.GT(), keychain: tr.Signing.Keychain()}
	putFile(t, w.bin, "old binary", 0o755)
	if err := os.MkdirAll(tr.Signing.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	putFile(t, w.keychain, "keychain", 0o600)
	putFile(t, tr.Signing.PasswordFile(), testPassword, 0o600)
	// The immutable flag's real implementation shells out to chflags/chattr;
	// these cases record that it was asked for instead (gt-tpg0p.11).
	tr.Files.ClearImmutable = func(path string) { w.cleared = append(w.cleared, path) }
	tr.Files.SetImmutable = func(path string) { w.set = append(w.set, path) }
	return w
}

// TestSign pins the whole contract of the best-effort signing step: every way
// it cannot sign leaves the install exactly as it was, one WARNING, and a sign
// that works is a copy, signed, then renamed over the installed binary
// (gt-tpg0p.11).
func TestSign(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(w *signWorld)
		respond func(dir, name string, args []string) ([]byte, []byte, int)
		// want is the WARNING the step logged, with BIN and KEYCHAIN standing
		// for the run's paths.
		want         string
		wantCmds     int
		wantSigned   bool
		wantNoBinary bool
		codesignName string
		// password is written to the password file, one line with the newline
		// keychain.pass carries; testPassword when empty.
		password string
		// wantUnlock is the whole line the unlock read on stdin, with KEYCHAIN
		// standing for the run's keychain. The default treats the password and
		// the keychain as words that need no quoting.
		wantUnlock string
	}{
		{
			name:         "nothing installed is not signed",
			setup:        func(w *signWorld) { _ = os.Remove(w.bin) },
			want:         "[install-gt] WARNING: not signing: nothing installed at BIN",
			wantNoBinary: true,
		},
		{
			name: "no signing material directory installs ad-hoc",
			// A run that found no town and was given no INSTALL_GT_SIGN_DIR has
			// no directory to look in; an empty Dir must not resolve to the
			// current directory, where a stray keychain and password file would
			// be signed with.
			setup: func(w *signWorld) { w.Signing.Dir = "" },
			want:  "[install-gt] WARNING: installing ad-hoc: no signing material directory (no town found; set INSTALL_GT_SIGN_DIR)",
		},
		{
			name:  "no keychain installs ad-hoc",
			setup: func(w *signWorld) { _ = os.Remove(w.keychain) },
			want:  "[install-gt] WARNING: installing ad-hoc: no keychain at KEYCHAIN",
		},
		{
			name:  "no password file installs ad-hoc",
			setup: func(w *signWorld) { _ = os.Remove(w.Signing.PasswordFile()) },
			want:  "[install-gt] WARNING: installing ad-hoc: no password file at PASSWORD",
		},
		{
			name: "a password file that cannot be read installs ad-hoc",
			setup: func(w *signWorld) {
				w.Files.ReadFile = func(string) ([]byte, error) { return nil, errors.New("unreadable") }
			},
			want: "[install-gt] WARNING: installing ad-hoc: cannot read PASSWORD",
		},
		{
			name:     "a keychain that will not unlock installs ad-hoc",
			respond:  func(dir, name string, args []string) ([]byte, []byte, int) { return nil, nil, 1 },
			want:     "[install-gt] WARNING: installing ad-hoc: cannot unlock KEYCHAIN",
			wantCmds: 1,
		},
		{
			name:     "a password spanning more than one line installs ad-hoc",
			password: "two\nlines",
			want:     "[install-gt] WARNING: installing ad-hoc: the password or the keychain path spans more than one line",
		},
		{
			name: "a temp file that cannot be created beside the binary installs ad-hoc",
			setup: func(w *signWorld) {
				// The material reads as present, but the binary sits in a
				// directory that does not exist, so mktemp for its copy fails.
				w.Files.IsFile = func(string) bool { return true }
				w.Files.ReadFile = func(string) ([]byte, error) { return []byte(testPassword), nil }
				w.Paths.BinDir = filepath.Join(filepath.Dir(w.Paths.BinDir), "missing", "bin")
				w.bin = w.Paths.GT()
			},
			want:     "[install-gt] WARNING: installing ad-hoc: cannot create a temp file beside BIN",
			wantCmds: 1,
		},
		{
			name: "a copy that cannot be written installs ad-hoc",
			setup: func(w *signWorld) {
				// mktemp hands back a directory, so the copy into it is the
				// step that fails.
				w.Files.TempFile = func(string, string) (string, error) {
					return filepath.Dir(w.bin), nil
				}
			},
			want:     "[install-gt] WARNING: installing ad-hoc: cannot copy BIN",
			wantCmds: 1,
		},
		{
			// The seam can only script statuses, so this stands in for an
			// unlock that reported success but did not take: the codesign that
			// needed the keychain fails, and the install goes ad-hoc with
			// exactly one WARNING (gt-4ucnp).
			name: "a codesign that fails after an unlock reporting success installs ad-hoc",
			respond: func(dir, name string, args []string) ([]byte, []byte, int) {
				if name == "codesign" {
					return nil, []byte("errSecInternalComponent\nmore detail\n"), 1
				}
				return nil, nil, 0
			},
			want:     "[install-gt] WARNING: installing ad-hoc: codesign failed: errSecInternalComponent",
			wantCmds: 2,
		},
		{
			name: "codesign that says nothing says so",
			respond: func(dir, name string, args []string) ([]byte, []byte, int) {
				if name == "codesign" {
					return nil, nil, 1
				}
				return nil, nil, 0
			},
			want:     "[install-gt] WARNING: installing ad-hoc: codesign failed: no output",
			wantCmds: 2,
		},
		{
			// The installer hands codesign's output over as stdout, so a failure
			// that reports only there is named by the first line of it too.
			name: "codesign that reports on stdout is named by the first line there",
			respond: func(dir, name string, args []string) ([]byte, []byte, int) {
				if name == "codesign" {
					return []byte("no identity found\nmore\n"), nil, 1
				}
				return nil, nil, 0
			},
			want:     "[install-gt] WARNING: installing ad-hoc: codesign failed: no identity found",
			wantCmds: 2,
		},
		{
			name: "a signed copy that cannot be renamed leaves the binary alone",
			setup: func(w *signWorld) {
				w.Files.Rename = func(string, string) error { return errors.New("read-only") }
			},
			want:     "[install-gt] WARNING: installing ad-hoc: cannot rename the signed copy over BIN",
			wantCmds: 2,
		},
		{
			name:       "a good sign replaces the binary with the signed copy",
			respond:    signsCopy,
			want:       "[install-gt] Signed BIN as com.gastown.gt",
			wantCmds:   2,
			wantSigned: true,
		},
		{
			name:       "a password with a space is quoted on the unlock's stdin",
			respond:    signsCopy,
			want:       "[install-gt] Signed BIN as com.gastown.gt",
			wantCmds:   2,
			wantSigned: true,
			password:   "two words",
			wantUnlock: `unlock-keychain -p "two words" KEYCHAIN`,
		},
		{
			name: "timeout bounds codesign where timeout(1) is on PATH",
			setup: func(w *signWorld) {
				w.Found = func(name string) bool { return name == "timeout" }
			},
			respond:      signsCopy,
			want:         "[install-gt] Signed BIN as com.gastown.gt",
			wantCmds:     2,
			wantSigned:   true,
			codesignName: "timeout",
		},
		{
			name: "gtimeout bounds codesign where timeout(1) is not linked",
			setup: func(w *signWorld) {
				w.Found = func(name string) bool { return name == "gtimeout" }
			},
			respond:      signsCopy,
			want:         "[install-gt] Signed BIN as com.gastown.gt",
			wantCmds:     2,
			wantSigned:   true,
			codesignName: "gtimeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSignWorld(t)
			if tc.password != "" {
				putFile(t, w.Signing.PasswordFile(), tc.password+"\n", 0o600)
			}
			if tc.setup != nil {
				tc.setup(w)
			}
			w.respond = tc.respond

			w.Sign()

			want := tc.want
			want = strings.ReplaceAll(want, "BIN", w.bin)
			want = strings.ReplaceAll(want, "KEYCHAIN", w.keychain)
			want = strings.ReplaceAll(want, "PASSWORD", w.Signing.PasswordFile())
			if got := w.lines(); !containsLine(got, want) {
				t.Errorf("stdout = %q, want the line %q", got, want)
			}
			if len(w.cmds) != tc.wantCmds {
				t.Fatalf("commands = %q, want %d", w.cmds, tc.wantCmds)
			}
			// Each way of not signing is exactly one WARNING and a sign that
			// works warns about nothing: an install never sees a warning and a
			// failure buried under a second one saying the same thing.
			warnings, wantWarnings := 0, 1
			if tc.wantSigned {
				wantWarnings = 0
			}
			for _, line := range w.lines() {
				if strings.Contains(line, "WARNING:") {
					warnings++
				}
			}
			if warnings != wantWarnings {
				t.Errorf("warnings = %d (%q), want exactly %d", warnings, w.lines(), wantWarnings)
			}
			// Every way of not signing leaves the binary it was handed exactly
			// as it was: the step is best effort, never a change it cannot
			// finish (gt-tpg0p.11).
			if !tc.wantSigned {
				w.checkUntouched(t, tc.wantNoBinary)
			}
			if tc.wantCmds == 0 {
				return
			}
			password := tc.password
			if password == "" {
				password = strings.TrimSpace(testPassword)
			}
			// The password reaches the unlock's stdin and nothing else: it is
			// never in the argv ps would show it in, never in the output and
			// never in codesign's argv (gt-4ucnp).
			if got, want := w.cmds[0], []string{"security", "-i"}; !reflect.DeepEqual(got, want) {
				t.Errorf("unlock argv = %q, want %q", got, want)
			}
			if args := strings.Join(w.cmds[0], " "); strings.Contains(args, password) {
				t.Errorf("unlock argv %q holds the password", w.cmds[0])
			}
			// The expectation is written out here rather than built with
			// quoteLine: an expectation computed by the code under test agrees
			// with it however that code is broken.
			wantUnlock := tc.wantUnlock
			if wantUnlock == "" {
				wantUnlock = "unlock-keychain -p " + password + " KEYCHAIN"
			}
			if strings.ContainsAny(w.keychain, " \t\"'\\") {
				t.Fatalf("the temporary keychain path %q needs quoting; this expectation is written for a bare word", w.keychain)
			}
			wantUnlock = strings.ReplaceAll(wantUnlock, "KEYCHAIN", w.keychain) + "\n"
			if got := w.stdins[0]; got != wantUnlock {
				t.Errorf("unlock stdin = %q, want %q", got, wantUnlock)
			}
			if out := w.out.String() + w.errOut.String(); strings.Contains(out, password) {
				t.Error("the password reached the output")
			}
			if tc.wantCmds < 2 {
				return
			}
			w.checkCodesign(t, tc.codesignName)
			if tc.wantSigned {
				// The installed file is the copy codesign rewrote, not the
				// binary it replaced, and it is executable: the mktemp that
				// held the copy was 0600.
				if got, want := read(t, w.bin), signedBinary; got != want {
					t.Errorf("installed binary = %q, want the signed copy %q", got, want)
				}
				info, err := os.Stat(w.bin)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0o111 == 0 {
					t.Errorf("installed binary mode = %v, want it executable", info.Mode().Perm())
				}
				if want := []string{w.bin}; !reflect.DeepEqual(w.set, want) {
					t.Errorf("immutable set on %q, want %q", w.set, want)
				}
				if len(w.cleared) != 2 {
					t.Errorf("immutable cleared on %q, want the temp copy and the binary", w.cleared)
				}
			}
			if leftovers := strayFiles(t, filepath.Dir(w.bin)); len(leftovers) != 0 {
				t.Errorf("temporary copies left behind: %q", leftovers)
			}
		})
	}
}

// TestCodesignArgvGuardsALeadingDash pins the codesign argv the installer runs:
// a keychain or a path that starts with a dash reaches codesign as ./-value, so
// codesign reads it as a path and never as an option. The expectations are
// written out, not built by the code under test (gt-qaa5m).
func TestCodesignArgvGuardsALeadingDash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		dir          string
		path         string
		wantKeychain string
		wantPath     string
	}{
		{
			name:         "ordinary keychain and path are unchanged",
			dir:          "/keys",
			path:         "/bin/.gt.signed.1",
			wantKeychain: "/keys/gastown-signing.keychain-db",
			wantPath:     "/bin/.gt.signed.1",
		},
		{
			name:         "a keychain in a dash-leading directory gets ./",
			dir:          "-keys",
			path:         "/bin/.gt.signed.1",
			wantKeychain: "./-keys/gastown-signing.keychain-db",
			wantPath:     "/bin/.gt.signed.1",
		},
		{
			name:         "a dash-leading path gets ./",
			dir:          "/keys",
			path:         "-gt.signed.1",
			wantKeychain: "/keys/gastown-signing.keychain-db",
			wantPath:     "./-gt.signed.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSignWorld(t)
			w.Signing.Dir = tc.dir
			if _, err := w.codesign(tc.path); err != nil {
				t.Fatalf("codesign: %v", err)
			}
			want := []string{"codesign", "--force", "--keychain", tc.wantKeychain, "-s", "gastown-dev-unattended", "-i", signIdentifier, tc.wantPath}
			if len(w.cmds) != 1 || !reflect.DeepEqual(w.cmds[0], want) {
				t.Errorf("codesign ran %q, want %q", w.cmds, want)
			}
		})
	}
}

// TestUnlockLineQuotesWhatTheParserNeeds pins the one line security -i reads:
// an ordinary word passes as it is, and a word holding a space or a quote is
// wrapped in double quotes with its backslashes and double quotes escaped. A
// form the parser cannot read fails the unlock — one WARNING and an ad-hoc
// install — rather than signing with a password it misread (gt-4ucnp).
func TestUnlockLineQuotesWhatTheParserNeeds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		password string
		keychain string
		want     string
		refused  bool
	}{
		{
			name:     "ordinary words pass as they are",
			password: "s3cret",
			keychain: "/material/k.keychain-db",
			want:     "unlock-keychain -p s3cret /material/k.keychain-db\n",
		},
		{
			name:     "a space is quoted",
			password: "two words",
			keychain: "/material/k.keychain-db",
			want:     `unlock-keychain -p "two words" /material/k.keychain-db` + "\n",
		},
		{
			name:     "a double quote is escaped inside the quotes",
			password: `a"b`,
			keychain: "/k",
			want:     `unlock-keychain -p "a\"b" /k` + "\n",
		},
		{
			name:     "a lone backslash is quoted and escaped",
			password: `a\b`,
			keychain: "/k",
			want:     `unlock-keychain -p "a\\b" /k` + "\n",
		},
		{
			name:     "a keychain path with a space is quoted too",
			password: "pw",
			keychain: "/my material/k",
			want:     `unlock-keychain -p pw "/my material/k"` + "\n",
		},
		{
			name:     "a password spanning two lines is refused",
			password: "a\nb",
			keychain: "/k",
			refused:  true,
		},
		{
			name:     "a keychain path spanning two lines is refused",
			password: "pw",
			keychain: "/a\nb",
			refused:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := unlockLine(tc.password, tc.keychain)
			if wantOK := !tc.refused; ok != wantOK {
				t.Errorf("unlockLine() ok = %v for %q, want %v", ok, tc.password, wantOK)
			}
			if got != tc.want {
				t.Errorf("unlockLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// checkCodesign asserts the codesign argv, with the temporary copy's path
// matched by its shape rather than its random name.
func (w *signWorld) checkCodesign(t *testing.T, name string) {
	t.Helper()
	got := w.cmds[1]
	if name == "" {
		name = "codesign"
	}
	if got[0] != name {
		t.Fatalf("codesign ran as %q, want %q", got[0], name)
	}
	if name != "codesign" {
		if got[1] != w.Signing.Timeout || got[2] != "codesign" {
			t.Errorf("bounds = %q, want %q then codesign", got[1:3], w.Signing.Timeout)
		}
		got = got[2:]
	}
	wantPrefix := []string{"codesign", "--force", "--keychain", w.keychain, "-s", w.Signing.Identity, "-i", signIdentifier}
	if !reflect.DeepEqual(got[:8], wantPrefix) {
		t.Errorf("codesign argv = %q, want it to start %q", got, wantPrefix)
	}
	signed := got[8]
	if dir, base := filepath.Dir(signed), filepath.Base(signed); dir != filepath.Dir(w.bin) || !strings.HasPrefix(base, ".gt.signed.") {
		t.Errorf("signed path = %q, want a .gt.signed.* copy beside %q", signed, w.bin)
	}
}

// checkUntouched asserts the installed binary is the file the case started
// with — same bytes, still executable — or, for a case that removed it, that
// nothing is there.
func (w *signWorld) checkUntouched(t *testing.T, removed bool) {
	t.Helper()
	info, err := os.Stat(w.installed)
	if removed {
		if !os.IsNotExist(err) {
			t.Errorf("installed binary %s still exists (%v), want none", w.installed, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if got := read(t, w.installed); got != "old binary" {
		t.Errorf("installed binary = %q, want the untouched %q", got, "old binary")
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode = %v, want it left executable", info.Mode().Perm())
	}
}

// TestResolveSigning pins where the signing material is looked for: the
// environment first, the town's own directory second, and nothing at all when
// the run found no town — never a path relative to wherever it happens to run.
func TestResolveSigning(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	cases := []struct {
		name     string
		getenv   func(string) string
		townRoot string
		want     Signing
	}{
		{
			name:     "a town's own signing directory",
			getenv:   env(nil),
			townRoot: "/town",
			want:     Signing{Dir: "/town/.runtime/signing", Identity: "gastown-dev-unattended", Timeout: "30"},
		},
		{
			name: "the environment overrides every default",
			getenv: env(map[string]string{
				"INSTALL_GT_SIGN_DIR":      "/material",
				"INSTALL_GT_SIGN_IDENTITY": "someone-else",
				"INSTALL_GT_SIGN_TIMEOUT":  "5",
			}),
			townRoot: "/town",
			want:     Signing{Dir: "/material", Identity: "someone-else", Timeout: "5"},
		},
		{
			name:   "no town leaves no directory",
			getenv: env(nil),
			want:   Signing{Dir: "", Identity: "gastown-dev-unattended", Timeout: "30"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveSigning(tc.getenv, tc.townRoot); got != tc.want {
				t.Errorf("ResolveSigning() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// strayFiles is every file in dir other than the installed binary, which is
// what a failed sign must not leave behind.
func strayFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.Name() != "gt" {
			files = append(files, entry.Name())
		}
	}
	return files
}
