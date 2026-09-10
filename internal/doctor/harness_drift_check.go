package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// harnessManifest mirrors the "files" section of the deploy-managed harness
// manifest at <rig>/.gastown-harness-manifest.json (written by
// contrib/gastown/deploy.sh in the om repo): a map of file path, relative to
// the rig root, to its expected sha256 hex digest. Doctor reads the manifest
// directly rather than depending on the om-invoker package's own manifest
// loader (internal/refinery/editorial), which governs `gt mq review`'s
// version assertion and is versioned independently of this drift check.
type harnessManifest struct {
	Files map[string]string `json:"files"`
}

// HarnessDriftCheck detects hand edits to deploy-managed harness files
// (scripts/om-gate.sh, formula overlays, directives) by comparing their
// current sha256 against the manifest deploy.sh wrote. Managed files are
// written only by deploy.sh; any other edit is drift.
type HarnessDriftCheck struct {
	BaseCheck
}

// NewHarnessDriftCheck creates a new harness-drift check.
func NewHarnessDriftCheck() *HarnessDriftCheck {
	return &HarnessDriftCheck{
		BaseCheck: BaseCheck{
			CheckName:        "harness-drift",
			CheckDescription: "Detect hand edits to deploy-managed harness files",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run compares each manifest-listed file's current sha256 against the
// recorded value.
func (c *HarnessDriftCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified",
		}
	}

	manifestPath := filepath.Join(rigPath, ".gastown-harness-manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "no manifest",
			Details: []string{fmt.Sprintf("expected %s", manifestPath)},
		}
	}

	var manifest harnessManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "no manifest",
			Details: []string{fmt.Sprintf("could not parse %s: %v", manifestPath, err)},
		}
	}

	if len(manifest.Files) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "no manifest",
			Details: []string{fmt.Sprintf("%s lists no managed files", manifestPath)},
		}
	}

	var drifted []string
	paths := make([]string, 0, len(manifest.Files))
	for path := range manifest.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		wantSHA := manifest.Files[path]
		actualSHA, err := sha256File(filepath.Join(rigPath, path))
		if err != nil {
			drifted = append(drifted, fmt.Sprintf("%s: missing (%v)", path, err))
			continue
		}
		if actualSHA != wantSHA {
			drifted = append(drifted, fmt.Sprintf("%s: sha mismatch", path))
		}
	}

	if len(drifted) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d managed file(s) drifted from harness manifest", len(drifted)),
			Details: drifted,
			FixHint: "Restore with the om repo's deploy.sh, or run deploy.sh --force to overwrite the hand edit",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("%d managed file(s) match harness manifest", len(paths)),
	}
}

// sha256File returns the hex-encoded sha256 digest of the file at path.
func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
