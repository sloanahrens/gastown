package manifestdata

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretFile is one entry of deploy/daemon/secrets.yaml, whose header defines
// the fields once. Name is the file's name in the config directory, never a
// path.
type SecretFile struct {
	Name     string `yaml:"name"`
	Consumer string `yaml:"consumer"`
	Mode     string `yaml:"mode"`
	Required bool   `yaml:"required"`
}

// UnclaimedSecret is one entry of the manifest's unclaimed list. The header of
// deploy/daemon/secrets.yaml defines what an unclaimed entry is and how it is
// used; the render records it and mounts nothing for it.
type UnclaimedSecret struct {
	Name   string `yaml:"name"`
	Search string `yaml:"search"`
}

// SecretsManifest is deploy/daemon/secrets.yaml parsed. The header of that file
// defines what the lists and fields mean and is the one place they are written
// down (gt-gg9hv.66).
type SecretsManifest struct {
	Files     []SecretFile      `yaml:"files"`
	Unclaimed []UnclaimedSecret `yaml:"unclaimed"`
}

// secretMode is the form of a manifest entry's mode. The manifest header defines
// the mode's meaning; this regexp is the form the parser accepts.
var secretMode = regexp.MustCompile(`^0[0-7]00$`)

// ParseSecretsManifest reads a secrets manifest. A key the format does not
// define, a second YAML document, an entry with no name, consumer or valid mode,
// a repeated name, or an unclaimed entry without the search that cleared it is
// refused, naming the entry.
func ParseSecretsManifest(data []byte) (SecretsManifest, error) {
	var m SecretsManifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return SecretsManifest{}, fmt.Errorf("secrets manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return SecretsManifest{}, errors.New("secrets manifest: holds more than one YAML document")
		}
		return SecretsManifest{}, fmt.Errorf("secrets manifest: %w", err)
	}
	if err := ValidateSecretsManifest(m); err != nil {
		return SecretsManifest{}, err
	}
	return m, nil
}

// ValidateSecretsManifest checks every entry of a manifest, or of a spec's
// Secrets list, which the render does not trust to have been parsed by the
// command.
func ValidateSecretsManifest(m SecretsManifest) error {
	if len(m.Files) == 0 {
		return errors.New("secrets manifest: lists no files; a manifest that mounts nothing is a mount of the whole config directory by another name")
	}
	listed := make(map[string]bool, len(m.Files))
	for i, f := range m.Files {
		entry := f.Name
		if entry == "" {
			entry = fmt.Sprintf("files[%d]", i)
		}
		if err := validateSecretName(f.Name); err != nil {
			return fmt.Errorf("secrets manifest: entry %q %w", entry, err)
		}
		if listed[f.Name] {
			return fmt.Errorf("secrets manifest: entry %q is listed more than once; two mounts would share a target", f.Name)
		}
		listed[f.Name] = true
		if strings.TrimSpace(f.Consumer) == "" {
			return fmt.Errorf("secrets manifest: entry %q has no consumer; name the daemon code or setting that reads it", f.Name)
		}
		if !secretMode.MatchString(f.Mode) {
			return fmt.Errorf("secrets manifest: entry %q mode %q is not a four-digit octal mode such as 0600", f.Name, f.Mode)
		}
	}
	unclaimed := make(map[string]bool, len(m.Unclaimed))
	for i, u := range m.Unclaimed {
		entry := u.Name
		if entry == "" {
			entry = fmt.Sprintf("unclaimed[%d]", i)
		}
		if err := validateSecretName(u.Name); err != nil {
			return fmt.Errorf("secrets manifest: unclaimed entry %q %w", entry, err)
		}
		if unclaimed[u.Name] {
			return fmt.Errorf("secrets manifest: unclaimed entry %q is listed more than once", u.Name)
		}
		unclaimed[u.Name] = true
		if listed[u.Name] {
			return fmt.Errorf("secrets manifest: %q is both a file the daemon reads and an unclaimed one", u.Name)
		}
		if strings.TrimSpace(u.Search) == "" {
			return fmt.Errorf("secrets manifest: unclaimed entry %q has no search; record the search that found no consumer", u.Name)
		}
	}
	return nil
}

// validateSecretName refuses a name that is not one plain file in the config
// directory. An absolute path, a separator, or a .. would make the joined path
// name something else, so each is refused rather than joined.
func validateSecretName(name string) error {
	switch {
	case name == "":
		return errors.New("has no name")
	case filepath.IsAbs(name) || strings.HasPrefix(name, "/"):
		return errors.New("is an absolute path; the manifest lists a file in the config directory")
	case strings.ContainsAny(name, `/\`):
		return errors.New("names a path, not a file in the config directory")
	case name == "." || name == "..":
		return errors.New("is . or .., which name no file in the config directory")
	case strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return errors.New("holds a control character, which no file name the render writes can carry")
	}
	return nil
}
