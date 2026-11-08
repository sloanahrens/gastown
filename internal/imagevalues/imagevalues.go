// Package imagevalues is the daemon and polecat image pair for one green
// commit: the registry and image names the images workflow publishes
// (gt-gg9hv.62), and the values file gt update images writes and the later
// deploy steps read (gt-gg9hv.63).
package imagevalues

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

const (
	// RegistryHost is the Forgejo instance that serves the registry and owns
	// the workflow's deploy host; the images workflow's FORGEJO_HOST and this
	// constant are held equal by TestImagesWorkflowNamesTheSharedImageConstants.
	RegistryHost = "forgejo:3000"
	// RegistryBase is the registry's HTTP root.
	RegistryBase = "http://" + RegistryHost
	// RegistryOwner is the account that owns both images.
	RegistryOwner = "sloan"
	// DaemonImage is the daemon image's name.
	DaemonImage = "gastown-daemon"
	// PolecatImage is the polecat image's name.
	PolecatImage = "gastown-polecat"
	// CommitLabel is the label each image carries for the commit it was built
	// from.
	CommitLabel = "org.gastown.gt-commit"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// valuesHeader marks the file generated, so nobody edits it by hand.
const valuesHeader = "# The daemon and polecat image pair for one green commit, written by gt update images. Do not edit.\n"

// Values is the image pair recorded for one commit.
type Values struct {
	// Daemon and Polecat are digest references, repo@sha256:<64 lowercase hex>.
	Daemon  string `yaml:"daemon"`
	Polecat string `yaml:"polecat"`
	// Commit is the full commit both images were built from.
	Commit string `yaml:"commit"`
}

// Repo is image's repository name on the registry.
func Repo(image string) string {
	return RegistryHost + "/" + RegistryOwner + "/" + image
}

// Ref is image's digest reference on the registry.
func Ref(image, digest string) string {
	return Repo(image) + "@" + digest
}

// ValidCommit reports whether sha is a full lowercase commit.
func ValidCommit(sha string) bool {
	return commitPattern.MatchString(sha)
}

// ValidDigest reports whether digest is sha256: followed by 64 lowercase hex.
func ValidDigest(digest string) bool {
	return digestPattern.MatchString(digest)
}

// Parse reads a values file's bytes, refusing one that is malformed rather than
// answering a partial pair.
func Parse(data []byte) (Values, error) {
	var v Values
	if err := yaml.Unmarshal(data, &v); err != nil {
		return Values{}, fmt.Errorf("imagevalues: the values file does not parse: %w", err)
	}
	if err := v.validate(); err != nil {
		return Values{}, err
	}
	return v, nil
}

// Load reads the values file at path. A missing file is an error wrapping
// fs.ErrNotExist; it is never read as an empty pair.
func Load(path string) (Values, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Values{}, err
	}
	return Parse(data)
}

// Write records v at path atomically, creating the directory the file names.
func Write(path string, v Values) error {
	if err := v.validate(); err != nil {
		return err
	}
	// The validated refs and commit hold no whitespace or YAML punctuation, so
	// the lines are written as plain scalars.
	data := fmt.Sprintf("%sdaemon: %s\npolecat: %s\ncommit: %s\n", valuesHeader, v.Daemon, v.Polecat, v.Commit)
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("imagevalues: create %s: %w", dir, err)
		}
	}
	if err := atomicfile.WriteFile(path, []byte(data), 0o644); err != nil {
		return fmt.Errorf("imagevalues: write %s: %w", path, err)
	}
	return nil
}

// validate refuses a pair that a consumer could not pull or that names no
// commit.
func (v Values) validate() error {
	for _, field := range []struct{ name, value string }{
		{"daemon", v.Daemon},
		{"polecat", v.Polecat},
		{"commit", v.Commit},
	} {
		if field.value == "" {
			return fmt.Errorf("imagevalues: the values file has no %s line", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{"daemon", v.Daemon},
		{"polecat", v.Polecat},
	} {
		if !refPattern.MatchString(field.value) {
			return fmt.Errorf("imagevalues: the values file's %s %q is not repo@sha256:<64 lowercase hex>", field.name, field.value)
		}
	}
	if !ValidCommit(v.Commit) {
		return fmt.Errorf("imagevalues: the values file's commit %q is not a full 40-character lowercase commit", v.Commit)
	}
	return nil
}
