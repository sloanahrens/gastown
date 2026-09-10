package editorial

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
)

// manifestFileName is the deploy-manifest file deploy.sh writes at the rig
// root, recording the sha256 and version of every managed harness file
// (contrib/gastown/deploy.sh, om-gate T14). gt mq review asserts against it
// so a stale or hand-edited binary/rubric is caught at review time instead
// of surfacing as an unexplained queue stall (be-52h class).
const manifestFileName = ".gastown-harness-manifest.json"

// Manifest is the deploy manifest's om-relevant subset: the om binary's
// location, content hash, and version, and the rubric file's location and
// content hash. Both are asserted by AssertVersion before every review.
type Manifest struct {
	OMBinary struct {
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
		Version string `json:"version"`
	} `json:"om_binary"`
	Rubric struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"rubric"`
	Files map[string]string `json:"files,omitempty"`
}

// LoadManifest reads and parses the harness manifest from rigDir.
func LoadManifest(rigDir string) (*Manifest, error) {
	path := filepath.Join(rigDir, manifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading harness manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing harness manifest %s: %w", path, err)
	}
	return &m, nil
}

// AssertVersion asserts, in order: the om binary is present and its content
// sha256 matches the manifest; the rubric file's content sha256 matches the
// manifest; and, when cfg.MinVersion is set, the manifest's recorded om
// version is not below it. "dev" (the unset-ldflags default) is treated as
// below any floor. A mismatch anywhere returns a *ClassifiedError classed
// BinaryMissing or VersionMismatch — callers must run this before invoking
// the gate script, per the fail-closed table's ordering.
func AssertVersion(m *Manifest, cfg config.EditorialConfig, rigRepoDir string) error {
	if m == nil {
		return &ClassifiedError{Class: BinaryMissing, Err: fmt.Errorf("no harness manifest loaded")}
	}
	if m.OMBinary.Path == "" {
		return &ClassifiedError{Class: BinaryMissing, Err: fmt.Errorf("manifest has no om_binary.path")}
	}
	info, err := os.Stat(m.OMBinary.Path)
	if err != nil {
		return &ClassifiedError{Class: BinaryMissing, Err: fmt.Errorf("om binary not found at %s: %w", m.OMBinary.Path, err)}
	}
	if info.IsDir() {
		return &ClassifiedError{Class: BinaryMissing, Err: fmt.Errorf("om binary path %s is a directory", m.OMBinary.Path)}
	}

	actualBinarySHA, err := sha256File(m.OMBinary.Path)
	if err != nil {
		return &ClassifiedError{Class: BinaryMissing, Err: fmt.Errorf("hashing om binary %s: %w", m.OMBinary.Path, err)}
	}
	if actualBinarySHA != m.OMBinary.SHA256 {
		return &ClassifiedError{Class: VersionMismatch, Err: fmt.Errorf("om binary sha256 %s does not match manifest %s", actualBinarySHA, m.OMBinary.SHA256)}
	}

	if m.Rubric.Path != "" {
		rubricPath := m.Rubric.Path
		if !filepath.IsAbs(rubricPath) {
			rubricPath = filepath.Join(rigRepoDir, m.Rubric.Path)
		}
		actualRubricSHA, err := sha256File(rubricPath)
		if err != nil {
			return &ClassifiedError{Class: VersionMismatch, Err: fmt.Errorf("hashing rubric %s: %w", rubricPath, err)}
		}
		if actualRubricSHA != m.Rubric.SHA256 {
			return &ClassifiedError{Class: VersionMismatch, Err: fmt.Errorf("rubric sha256 %s does not match manifest %s", actualRubricSHA, m.Rubric.SHA256)}
		}
	}

	if cfg.MinVersion != "" {
		v := m.OMBinary.Version
		if v == "" || v == "dev" {
			return &ClassifiedError{Class: VersionMismatch, Err: fmt.Errorf("om version %q is below min_version %q", v, cfg.MinVersion)}
		}
		if deps.CompareVersions(v, cfg.MinVersion) < 0 {
			return &ClassifiedError{Class: VersionMismatch, Err: fmt.Errorf("om version %s is below min_version %s", v, cfg.MinVersion)}
		}
	}

	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
