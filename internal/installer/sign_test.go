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

// signWorld is a run with a binary installed and the town's signing material
// beside it: a keychain and the 0600 password file that unlocks it. A case
// removes or breaks the part it is about.
type signWorld struct {
	*testRun
	bin      string
	keychain string
	cleared  []string
	set      []string
}

func newSignWorld(t *testing.T) *signWorld {
	t.Helper()
	tr := newTestRun(t)
	w := &signWorld{testRun: tr, bin: tr.Paths.GT(), keychain: tr.Signing.Keychain()}
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
		codesignName string
	}{
		{
			name:  "nothing installed is not signed",
			setup: func(w *signWorld) { _ = os.Remove(w.bin) },
			want:  "[install-gt] WARNING: not signing: nothing installed at BIN",
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
			name: "codesign that fails leaves the binary alone",
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
			name: "a signed copy that cannot be renamed leaves the binary alone",
			setup: func(w *signWorld) {
				w.Files.Rename = func(string, string) error { return errors.New("read-only") }
			},
			want:     "[install-gt] WARNING: installing ad-hoc: cannot rename the signed copy over BIN",
			wantCmds: 2,
		},
		{
			name:       "a good sign replaces the binary with the signed copy",
			want:       "[install-gt] Signed BIN as com.gastown.gt",
			wantCmds:   2,
			wantSigned: true,
		},
		{
			name: "timeout bounds codesign where timeout(1) is on PATH",
			setup: func(w *signWorld) {
				w.Found = func(name string) bool { return name == "timeout" }
			},
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
			if tc.wantCmds == 0 {
				return
			}
			// The password reaches the unlock and nothing else: it is never in
			// the output and never in codesign's argv.
			if out := w.out.String() + w.errOut.String(); strings.Contains(out, strings.TrimSpace(testPassword)) {
				t.Error("the password reached the output")
			}
			if got, want := w.cmds[0], []string{"security", "unlock-keychain", "-p", strings.TrimSpace(testPassword), w.keychain}; !reflect.DeepEqual(got, want) {
				t.Errorf("unlock argv = %q, want %q", got, want)
			}
			if tc.wantCmds < 2 {
				return
			}
			w.checkCodesign(t, tc.codesignName)
			if tc.wantSigned {
				if got, want := read(t, w.bin), "old binary"; got != want {
					t.Errorf("installed binary = %q, want %q", got, want)
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
