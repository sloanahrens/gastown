package podengine

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// updateLines rewrites the document's Line column from each row's quoted text
// before the tests run, so the strict run that follows sees the new lines.
var updateLines = flag.Bool("update-lines", false, "rewrite the Line column of "+engineDoc+" from each row's quoted text, printing each change")

func TestMain(m *testing.M) {
	flag.Parse()
	if *updateLines {
		if err := updateEngineDocLines(repoRoot, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "podengine: -update-lines: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(unittier.Main(m))
}
