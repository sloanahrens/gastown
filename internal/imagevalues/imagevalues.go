// Package imagevalues is the daemon and polecat image pair for one green
// commit: the values file gt update images writes and the later deploy steps
// read (gt-gg9hv.63). The Registry a pair lives in is passed in by the
// command: this package names the two images and the commit label, never a
// fork's registry host or account.
package imagevalues

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

const (
	// DaemonImage is the daemon image's name.
	DaemonImage = "gastown-daemon"
	// PolecatImage is the polecat image's name.
	PolecatImage = "gastown-polecat"
	// CommitLabel is the label each image carries for the commit it was built
	// from.
	CommitLabel = "org.gastown.gt-commit"
	// DefaultRelPath is the values file, relative to the rig's repository root:
	// gt update images writes it there and the consumers read it from there.
	DefaultRelPath = "deploy/values/images.yaml"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// hostPattern is a registry host name and optional port: what a values
	// line may hold in the place an image reference names its registry. A
	// bracketed IPv6 literal is refused: a ref beginning with '[' is not a
	// plain YAML scalar, so the writer could not emit it.
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)
	// ownerPattern is a registry account name: one path component, as
	// distribution references require.
	ownerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// Registry is where a town publishes its images: the scheme and host its
// Forgejo answers by, and the account that owns both images. The command reads
// it from the town's own Forgejo settings and passes it in, so a fork's host
// and owner live in that fork's config, never here.
type Registry struct {
	// Scheme is how the town reaches the registry: https, or http only where
	// the town's Forgejo has no https endpoint.
	Scheme string
	// Host is the registry's host and optional port, e.g. "registry.internal:5000".
	Host string
	// Owner is the account that owns both images.
	Owner string
}

// BaseURL is the registry's HTTP root.
func (r Registry) BaseURL() string { return r.Scheme + "://" + r.Host }

// Repo is image's repository name on the registry.
func (r Registry) Repo(image string) string {
	return r.Host + "/" + r.Owner + "/" + image
}

// Ref is image's digest reference on the registry.
func (r Registry) Ref(image, digest string) string {
	return r.Repo(image) + "@" + digest
}

// Validate refuses a registry whose parts would not reach a URL path or a
// values line as typed: the host must be a bare host:port and the owner one
// lowercase path component, or a value built from them could carry a query or
// a path of its own, or an uppercase letter a distribution reference rejects.
func (r Registry) Validate() error {
	switch r.Scheme {
	case "https", "http":
	default:
		return fmt.Errorf("imagevalues: the registry scheme %q is not https or http", r.Scheme)
	}
	if !hostPattern.MatchString(r.Host) {
		if strings.HasPrefix(r.Host, "[") {
			return fmt.Errorf("imagevalues: the registry host %q is a bracketed IPv6 literal, which a values line cannot hold as a plain scalar", r.Host)
		}
		return fmt.Errorf("imagevalues: the registry host %q is not a bare host:port", r.Host)
	}
	if !ownerPattern.MatchString(r.Owner) {
		return fmt.Errorf("imagevalues: the registry owner %q is not one lowercase account name", r.Owner)
	}
	return nil
}

// Values is the image pair recorded for one commit.
type Values struct {
	// Daemon and Polecat are digest references, repo@sha256:<64 lowercase hex>.
	Daemon  string `yaml:"daemon"`
	Polecat string `yaml:"polecat"`
	// Commit is the full commit both images were built from.
	Commit string `yaml:"commit"`
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

// LoadIn reads the values file DefaultRelPath under dir. A missing file answers
// false with no error: no pair has been built yet, which is not a failure. A
// file that exists and cannot be read or parsed is an error, never an absent
// pair. dir is the caller's to name; this package reads no setting for it.
func LoadIn(dir string) (Values, bool, error) {
	if dir == "" {
		return Values{}, false, fmt.Errorf("imagevalues: no directory to read %s from", DefaultRelPath)
	}
	v, err := Load(filepath.Join(dir, DefaultRelPath))
	if errors.Is(err, fs.ErrNotExist) {
		return Values{}, false, nil
	}
	if err != nil {
		return Values{}, false, err
	}
	return v, true, nil
}

// valuesHeader marks the file generated, so nobody edits it by hand.
const valuesHeader = "# The daemon and polecat image pair for one green commit, written by gt update images. Do not edit.\n"

// Write records v at path atomically, creating the directory the file names.
func Write(path string, v Values) error {
	if err := v.validate(); err != nil {
		return err
	}
	// validate admits only these characters: in a ref, lowercase letters,
	// digits and '.', '_', ':', '/', '-', '@'; in the commit, lowercase hex.
	// No value holds a space or starts with a YAML indicator, so each is
	// written as a plain scalar and needs no quoting.
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
