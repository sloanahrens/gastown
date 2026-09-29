// gen-bd-tree reads `bd capabilities --json` on stdin and writes the reduced
// snapshot that internal/cmdtree embeds. Run it through
// scripts/refresh-bd-command-tree.sh (make bd-command-tree), which builds bd
// from a beads checkout into a temporary directory first.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/cmdtree/bdsnapshot"
)

func main() {
	source := flag.String("source", "", "where the bd binary was built from (recorded in the snapshot)")
	flag.Parse()
	if *source == "" {
		fmt.Fprintln(os.Stderr, "gen-bd-tree: -source is required")
		os.Exit(2)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-bd-tree:", err)
		os.Exit(1)
	}
	snap, err := bdsnapshot.Reduce(raw, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-bd-tree:", err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-bd-tree:", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(append(out, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "gen-bd-tree:", err)
		os.Exit(1)
	}
}
