// gen-bd-tree writes the bd command-tree snapshot that internal/cmdtree
// embeds. It runs `bd capabilities --json` and `bd <parent> --help` for each
// parent command (to learn which parents take positional arguments) with the
// given bd binary, in an empty working directory; neither opens a store.
// scripts/refresh-bd-command-tree.sh (make bd-command-tree) builds that
// binary from a beads checkout first.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cmdtree/bdsnapshot"
)

func main() {
	bd := flag.String("bd", "", "bd binary to describe")
	source := flag.String("source", "", "where the bd binary was built from (recorded in the snapshot)")
	flag.Parse()
	if *bd == "" || *source == "" {
		fmt.Fprintln(os.Stderr, "gen-bd-tree: -bd and -source are required")
		os.Exit(2)
	}
	if err := run(*bd, *source); err != nil {
		fmt.Fprintln(os.Stderr, "gen-bd-tree:", err)
		os.Exit(1)
	}
}

func run(bd, source string) error {
	dir, err := os.MkdirTemp("", "gen-bd-tree-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// NewPlain with WithBin is the call the explicit argv made: the named
	// binary, the empty directory, the caller's environment. A failure returns
	// a CLIError whose text is bd's stderr, so main still prints bd's own words.
	bdStore := beads.NewPlain(dir, append(os.Environ(), "BD_DISABLE_METRICS=1"), beads.WithBin(bd))

	raw, err := bdStore.Run("capabilities", "--json")
	if err != nil {
		return fmt.Errorf("bd capabilities --json: %w", err)
	}
	snap, err := bdsnapshot.Reduce(raw, source)
	if err != nil {
		return err
	}
	help := func(path string) (string, error) {
		out, err := bdStore.Run(append(strings.Fields(path), "--help")...)
		return string(out), err
	}
	if err := bdsnapshot.MarkSubcommandOnly(&snap, help); err != nil {
		return err
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(out, '\n'))
	return err
}
