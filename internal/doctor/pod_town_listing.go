package doctor

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/townstate"
)

// PodTownListing returns the claim listing of the town at townRoot: one line per
// town copy file it holds, in PodTownListingFormat, which parsePodTownListing
// reads back. gt town claim-listing prints it for the Mac-side mac-town-state
// check (gt-xi5pa1). An unreadable registry or town file, or a root holding none
// of the copy's files, is an error rather than a shorter listing that would read
// as files the claim lacks.
func PodTownListing(townRoot string) ([]byte, error) {
	registry, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("reading the rig registry: %w", err)
	}
	rigs := make([]string, 0, len(registry.Rigs))
	for name := range registry.Rigs {
		if !validRigSegment(name) {
			return nil, fmt.Errorf("rig registry names %q, which is not a single path segment", name)
		}
		rigs = append(rigs, name)
	}
	sort.Strings(rigs)

	names, err := townstate.TownCopyPaths(os.DirFS(townRoot), townstate.TownCopyRows(rigs))
	if err != nil {
		return nil, fmt.Errorf("listing the town's copy rows: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("the town at %s holds none of the town copy's files", townRoot)
	}

	lines := make([]string, 0, len(names))
	for _, name := range names {
		f, err := readMacTownFile(townRoot, name)
		if err != nil {
			return nil, fmt.Errorf("reading town file %s: %w", name, err)
		}
		lines = append(lines, fmt.Sprintf("%s\t%d\t%s", f.sum, f.mtime.Unix(), name))
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
