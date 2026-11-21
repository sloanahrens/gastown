package git

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// showFileOutcome is what each git call answers in a scripted ShowFileAtRev:
// the rev-parse that resolves the ref, the cat-file that finds the path, and
// the show that reads it.
type showFileOutcome struct {
	revParse, catFile, show error
	content                 string
}

// showFileGit is a Git whose git calls are answered from o, so the three
// branches of ShowFileBytesAtRev run without starting git.
func showFileGit(t *testing.T, o showFileOutcome) *Git {
	return &Git{workDir: t.TempDir(), exec: func(c gitCall) (string, string, error) {
		switch c.args[0] {
		case "rev-parse":
			return "", "", o.revParse
		case "cat-file":
			return "", "", o.catFile
		case "show":
			return o.content, "", o.show
		}
		return "", "", errors.New("unscripted git call: " + c.args[0])
	}}
}

// exitStatus is a git failure that carries the exit code git exited with.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

func TestShowFileBytesAtRevReturnsExactBytes(t *testing.T) {
	t.Parallel()
	g := showFileGit(t, showFileOutcome{content: "prefix: tk\n\n"})
	got, err := g.ShowFileBytesAtRev("main", ".beads/config.yaml")
	if err != nil || string(got) != "prefix: tk\n\n" {
		t.Fatalf("ShowFileBytesAtRev = %q, %v; want the exact bytes", got, err)
	}
	text, err := g.ShowFileAtRev("main", ".beads/config.yaml")
	if err != nil || text != "prefix: tk" {
		t.Fatalf("ShowFileAtRev = %q, %v; want the trimmed text", text, err)
	}
}

func TestShowFileBytesAtRevErrors(t *testing.T) {
	t.Parallel()
	gitFailed := errors.New("git failed")
	for name, tc := range map[string]struct {
		outcome showFileOutcome
		// notAtRef is true when the path is absent: the answer, not a failure.
		notAtRef bool
		// refNotFound is true when rev-parse exited 1: the ref does not resolve.
		refNotFound bool
		// wantPrefix is how the message of an error from the resolve step begins,
		// whichever way rev-parse failed; the later steps' errors have none.
		wantPrefix string
	}{
		"unresolved ref is a real error": {
			outcome:    showFileOutcome{revParse: gitFailed},
			wantPrefix: "resolving main: ",
		},
		"rev-parse exit 1 is ErrRefNotFound": {
			outcome:     showFileOutcome{revParse: exitStatus(1)},
			refNotFound: true,
			wantPrefix:  "resolving main: ",
		},
		"rev-parse exit 128 is not ErrRefNotFound": {
			outcome:    showFileOutcome{revParse: exitStatus(128)},
			wantPrefix: "resolving main: ",
		},
		"absent path is ErrNotAtRef": {
			outcome:  showFileOutcome{catFile: gitFailed},
			notAtRef: true,
		},
		"failed read is a real error": {
			outcome: showFileOutcome{show: gitFailed},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := showFileGit(t, tc.outcome)
			for label, read := range map[string]func() error{
				"ShowFileBytesAtRev": func() error { _, err := g.ShowFileBytesAtRev("main", "x"); return err },
				"ShowFileAtRev":      func() error { _, err := g.ShowFileAtRev("main", "x"); return err },
			} {
				err := read()
				if err == nil {
					t.Fatalf("%s succeeded, want an error", label)
				}
				if got := errors.Is(err, ErrNotAtRef); got != tc.notAtRef {
					t.Errorf("%s error is ErrNotAtRef = %v, want %v (err: %v)", label, got, tc.notAtRef, err)
				}
				if got := errors.Is(err, ErrRefNotFound); got != tc.refNotFound {
					t.Errorf("%s error is ErrRefNotFound = %v, want %v (err: %v)", label, got, tc.refNotFound, err)
				}
				if !strings.HasPrefix(err.Error(), tc.wantPrefix) {
					t.Errorf("%s error message %q does not begin %q", label, err.Error(), tc.wantPrefix)
				}
			}
		})
	}
}
