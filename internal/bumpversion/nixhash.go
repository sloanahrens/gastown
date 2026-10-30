package bumpversion

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Exec is the process and scratch-directory seam for the nix step. Run executes
// name in dir with the entries in env added to the inherited environment, and
// returns what it wrote to standard output and to standard error. LookPath reports
// an error when file is not on PATH. MkdirTemp makes the scratch directory the
// vendor tree goes in, as os.MkdirTemp does.
type Exec struct {
	Run       func(dir string, env []string, name string, args ...string) (stdout, stderr []byte, err error)
	LookPath  func(file string) (string, error)
	MkdirTemp func(dir, pattern string) (string, error)
}

// VendorHash is what UpdateVendorHash did. Skipped is set when nix is not on PATH:
// flake.nix was neither read nor written. Otherwise Old is the vendorHash flake.nix
// held before and New the one it holds now, so Old == New means unchanged.
type VendorHash struct {
	Skipped bool
	Old     string
	New     string
}

// NixSkippedNote is the line the release summary prints when nix is not on PATH.
const NixSkippedNote = "flake.nix (skipped - nix not in PATH)"

// nixSkipLine is written to the caller when the step is skipped, so the operator
// sees that the hash was not refreshed even though the command exits 0.
const nixSkipLine = "nix not found: flake.nix vendorHash NOT updated"

// vendorHashMarker starts the line that holds the Go module's vendorHash.
const vendorHashMarker = "vendorHash = "

// sriSHA256 is the form nix hash path prints: sha256 in SRI base64 over 32 bytes,
// which is 43 characters and one '=' of padding after the prefix. Anything else is
// refused before it can reach flake.nix.
var sriSHA256 = regexp.MustCompile(`^sha256-[A-Za-z0-9+/]{43}=$`)

// hashPrefixLimit is how many bytes of a malformed hash an error names.
const hashPrefixLimit = 20

// stderrLimit is how many bytes of nix's standard error an error message keeps.
const stderrLimit = 200

// UpdateVendorHash recomputes the Go module's vendorHash in flake.nix and reports
// the old and new hash. When nix is not on PATH it writes nixSkipLine to out and
// returns Skipped, reading and writing nothing. A missing flake.nix, a flake.nix
// with no vendorHash line, a failed go mod vendor, and a hash nix does not print
// in SRI sha256 form are errors, and flake.nix is left as it was.
func UpdateVendorHash(fsys FS, run Exec, repoRoot string, out io.Writer) (VendorHash, error) {
	// nix is checked before flake.nix is read, so a skip touches nothing.
	if _, err := run.LookPath("nix"); err != nil {
		fmt.Fprintln(out, nixSkipLine)
		return VendorHash{Skipped: true}, nil
	}
	flake, err := readText(fsys, FlakePath)
	if errors.Is(err, fs.ErrNotExist) {
		return VendorHash{}, errors.New("Must run from repository root (flake.nix not found)")
	}
	if err != nil {
		return VendorHash{}, err
	}
	old := quotedValue(flake, vendorHashMarker)
	if old == "" {
		return VendorHash{}, errors.New("no vendorHash in flake.nix")
	}

	tmp, err := run.MkdirTemp("", "gt-vendor-")
	if err != nil {
		return VendorHash{}, fmt.Errorf("make temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	vendor := filepath.Join(tmp, "vendor")
	if err := goModVendor(run, repoRoot, vendor); err != nil {
		return VendorHash{}, err
	}
	stdout, stderr, err := run.Run(repoRoot, nil, "nix", "hash", "path", vendor)
	if err != nil {
		return VendorHash{}, fmt.Errorf("Could not compute vendorHash: nix hash path: %w (stderr: %s)", err, scrub(stderr))
	}
	hash := strings.TrimSpace(string(stdout))
	if hash == "" {
		return VendorHash{}, errors.New("Could not compute vendorHash")
	}
	// Refused before the write: stray output from nix must never reach flake.nix.
	if !sriSHA256.MatchString(hash) {
		return VendorHash{}, fmt.Errorf("Could not compute vendorHash: nix hash path printed %q, which is not a sha256 SRI hash", hashPrefix(hash))
	}

	if hash != old {
		edited := strings.ReplaceAll(flake, vendorHashMarker+`"`+old+`"`, vendorHashMarker+`"`+hash+`"`)
		if err := fsys.WriteFile(FlakePath, []byte(edited)); err != nil {
			return VendorHash{}, fmt.Errorf("write %s: %w", FlakePath, err)
		}
	}
	return VendorHash{Old: old, New: hash}, nil
}

// goModVendor writes the module tree into vendor. It runs with GOFLAGS=-mod=readonly
// and, when that fails, once more with the inherited environment, which is the
// script's `GOFLAGS=... go mod vendor || go mod vendor`. The output of a failed
// attempt is not in the error, since it can hold anything the module tool printed.
func goModVendor(run Exec, repoRoot, vendor string) error {
	args := []string{"mod", "vendor", "-o", vendor}
	if _, _, err := run.Run(repoRoot, []string{"GOFLAGS=-mod=readonly"}, "go", args...); err == nil {
		return nil
	}
	if _, _, err := run.Run(repoRoot, nil, "go", args...); err != nil {
		return fmt.Errorf("go mod vendor: %w", err)
	}
	return nil
}

// hashPrefix returns the first hashPrefixLimit bytes of a hash that failed the
// SRI check, so the error can name what nix printed without echoing all of it.
func hashPrefix(hash string) string {
	if len(hash) > hashPrefixLimit {
		return hash[:hashPrefixLimit]
	}
	return hash
}

// scrub returns nix's standard error as one line of at most stderrLimit bytes, so
// the error message stays one line however long nix's output runs.
func scrub(text []byte) string {
	line := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, string(text))
	line = strings.TrimSpace(line)
	if len(line) > stderrLimit {
		line = line[:stderrLimit]
	}
	return strings.ToValidUTF8(line, "")
}
