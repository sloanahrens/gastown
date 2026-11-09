package podengine

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// updateLines rewrites the document's Line column from each row's quoted text
// before the tests run, so the strict run that follows sees the new lines.
var updateLines = flag.Bool("update-lines", false, "rewrite the Line column of "+engineDoc+" from each row's quoted text, printing each change")

func TestMain(m *testing.M) {
	flag.Parse()
	if *updateLines {
		if status := runUpdateLines(repoRoot, os.Stdout, os.Stderr); status != 0 {
			os.Exit(status)
		}
	}
	os.Exit(unittier.Main(m))
}

// runUpdateLines runs the -update-lines rewrite and returns the exit status: 0
// when the rewrite ran, or 1 with the failure printed to errw when it could not.
func runUpdateLines(root string, w, errw io.Writer) int {
	if err := updateEngineDocLines(root, w); err != nil {
		fmt.Fprintf(errw, "podengine: -update-lines: %v\n", err)
		return 1
	}
	return 0
}

// TestRunUpdateLinesExitStatus holds the flag's exit status: a rewrite that can
// read the document exits 0, and one that cannot exits 1 with the failure on
// stderr (gt-lrkmdj).
func TestRunUpdateLinesExitStatus(t *testing.T) {
	t.Parallel()
	t.Run("a document that is read exits 0", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": updateFixtureSource,
			engineDoc:         renderEngineDoc(updateFixtureRow(4)),
		})
		var out, errw bytes.Buffer
		if status := runUpdateLines(root, &out, &errw); status != 0 {
			t.Fatalf("status = %d, want 0; stderr %q", status, errw.String())
		}
	})
	t.Run("a tree without the document exits 1 and names the failure on stderr", func(t *testing.T) {
		t.Parallel()
		var out, errw bytes.Buffer
		if status := runUpdateLines(t.TempDir(), &out, &errw); status != 1 {
			t.Fatalf("status = %d, want 1", status)
		}
		if !strings.HasPrefix(errw.String(), "podengine: -update-lines: ") {
			t.Errorf("stderr = %q, want the -update-lines failure", errw.String())
		}
	})
}
