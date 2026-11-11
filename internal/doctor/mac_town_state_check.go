package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/townstate"
)

// MacTownStateCheckName is the name 'gt doctor --check' takes for MacTownStateCheck.
const MacTownStateCheckName = "mac-town-state"

// MacTownStateCheck compares the town files a Mac town would need with the pod
// claim's listing, so a file the claim lacks is found before the cutover rather
// than by accident.
//
// The file list is the town copy's table (townstate.TownCopyRows), so the two
// cannot drift. The listing holds one line per file in the claim:
// '<sha256 hex>', a tab, '<mtime unix seconds>', a tab, and the path relative to
// the town root. A file the claim lacks is an error; a file whose bytes differ is
// a warning that names the newer side.
type MacTownStateCheck struct {
	BaseCheck

	// listing is the path of the pod claim's listing file.
	listing string
}

// NewMacTownStateCheck creates the check against the pod claim listing at listing.
func NewMacTownStateCheck(listing string) *MacTownStateCheck {
	return &MacTownStateCheck{
		BaseCheck: BaseCheck{
			CheckName:        MacTownStateCheckName,
			CheckDescription: "Compare the Mac town's configuration files with the pod claim listed by --pod-town",
			CheckCategory:    CategoryConfig,
		},
		listing: listing,
	}
}

// Run lists the town files the pod claim lacks and the ones it holds with other bytes.
func (c *MacTownStateCheck) Run(ctx *CheckContext) *CheckResult {
	data, err := os.ReadFile(c.listing)
	if err != nil {
		return c.result(StatusError, "Cannot read the pod claim listing "+c.listing,
			[]string{err.Error()}, "Write the listing from the pod's claim and pass its path to --pod-town")
	}
	pod, err := parsePodTownListing(data)
	if err != nil {
		return c.result(StatusError, "The pod claim listing is malformed", []string{err.Error()},
			"Each line is '<sha256>', a tab, '<mtime unix seconds>', a tab, and a path relative to the town root")
	}

	registry, err := config.LoadRigsConfigOrEmpty(constants.MayorRigsPath(ctx.TownRoot))
	if err != nil {
		return c.result(StatusError, "Cannot read the rig registry, so the town's rig rows are unknown",
			[]string{err.Error()}, "Repair mayor/rigs.json and re-run the check")
	}
	rigs := make([]string, 0, len(registry.Rigs))
	for name := range registry.Rigs {
		if !validRigSegment(name) {
			return c.result(StatusError, "The rig registry names a rig that is not a single path segment",
				[]string{fmt.Sprintf("rig name %q", name)}, "Rename the rig in mayor/rigs.json")
		}
		rigs = append(rigs, name)
	}
	sort.Strings(rigs)

	root := os.DirFS(ctx.TownRoot)
	names, err := townstate.TownCopyPaths(root, townstate.TownCopyRows(rigs))
	if err != nil {
		return c.result(StatusError, "Cannot list the Mac town's copy rows", []string{err.Error()},
			"Repair the unlistable directory under the Mac town and re-run the check")
	}
	if len(names) == 0 {
		return c.result(StatusError, "The Mac town holds none of the town copy's files, so nothing was compared",
			[]string{"town root " + ctx.TownRoot}, "Check that --rig and the town root point at the Mac town")
	}

	var missing, differ []string
	for _, name := range names {
		mac, err := readMacTownFile(ctx.TownRoot, name)
		if err != nil {
			return c.result(StatusError, "Cannot read a Mac town file the copy carries", []string{err.Error()},
				"Make the file a regular file and re-run the check")
		}
		claim, ok := pod[name]
		if !ok {
			missing = append(missing, fmt.Sprintf("missing on pod: %s (Mac mtime %s)",
				name, mac.mtime.UTC().Format(time.RFC3339)))
			continue
		}
		if mac.sum != claim.sum {
			differ = append(differ, fmt.Sprintf("differs: %s (%s)", name, newerSide(mac.mtime, claim.mtime)))
		}
	}

	switch {
	case len(missing) > 0:
		return c.result(StatusError,
			fmt.Sprintf("%d town file(s) the pod claim lacks", len(missing)),
			append(missing, differ...),
			"Copy the listed files into the pod claim before the cutover, then re-run with --pod-town")
	case len(differ) > 0:
		return c.result(StatusWarning,
			fmt.Sprintf("%d town file(s) differ from the pod claim", len(differ)),
			differ,
			"Carry the newer side's copy into the pod claim before the cutover, then re-run with --pod-town")
	}
	return c.result(StatusOK,
		fmt.Sprintf("The pod claim holds all %d town file(s) the Mac town copy carries", len(names)), nil, "")
}

func (c *MacTownStateCheck) result(status CheckStatus, msg string, details []string, hint string) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  status,
		Message: msg,
		Details: details,
		FixHint: hint,
	}
}

// podTownFile is one line of the pod claim listing.
type podTownFile struct {
	sum   string
	mtime time.Time
}

// parsePodTownListing reads the listing. A malformed line is an error that names
// its line number, never a skipped line: a skipped file reads as missing from the
// claim and would hide a real gap. The paths come from the pod, so each one is
// checked as a local path before it is joined to the Mac town root.
func parsePodTownListing(data []byte) (map[string]podTownFile, error) {
	files := make(map[string]podTownFile)
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := i + 1
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: want 3 tab-separated fields, got %d", n, len(fields))
		}
		if len(fields[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("line %d: sha256 is not %d hex digits", n, sha256.Size*2)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("line %d: sha256 is not hex", n)
		}
		secs, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: mtime is not unix seconds", n)
		}
		name := fields[2]
		if !fs.ValidPath(name) || name == "." {
			return nil, fmt.Errorf("line %d: path is not a relative path inside the town", n)
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("line %d: path listed twice", n)
		}
		files[name] = podTownFile{sum: strings.ToLower(fields[0]), mtime: time.Unix(secs, 0)}
	}
	return files, nil
}

// macTownFile is one Mac town file the copy carries.
type macTownFile struct {
	sum   string
	mtime time.Time
}

// readMacTownFile hashes one regular file under the Mac town root. A file that is
// not regular is an error, so a directory in a file's place is never compared as
// clean.
func readMacTownFile(townRoot, name string) (macTownFile, error) {
	p := filepath.Join(townRoot, filepath.FromSlash(name))
	info, err := os.Stat(p)
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	var data []byte
	if err == nil {
		data, err = os.ReadFile(p)
	}
	if err != nil {
		return macTownFile{}, err
	}
	sum := sha256.Sum256(data)
	return macTownFile{sum: hex.EncodeToString(sum[:]), mtime: info.ModTime()}, nil
}

// newerSide names which copy was written last, by whole seconds, since the
// listing carries seconds.
func newerSide(mac, claim time.Time) string {
	switch {
	case mac.Unix() > claim.Unix():
		return "the Mac copy is newer"
	case claim.Unix() > mac.Unix():
		return "the pod copy is newer"
	}
	return "same mtime"
}

// validRigSegment reports whether a rig name is one path segment, since the copy
// table joins it to the town root.
func validRigSegment(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`)
}
