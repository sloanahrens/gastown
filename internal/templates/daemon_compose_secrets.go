package templates

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretsRecreateNotice is printed with every manifest render and written in the
// header of deploy/daemon/secrets.yaml. A bind mount of one file pins that
// file's inode, so a secret replaced by rename (an editor's atomic save, mv)
// stays the old file inside the running container; a rewrite in place does not.
// A Kubernetes Secret volume has no such property, so this is a compose-only
// cost (gt-gg9hv.66, gt-gg9hv.30).
const SecretsRecreateNotice = "rotating any listed secret requires recreating the daemon service (a bind mount of one file pins its inode)"

// SecretFS is the one seam through which the secrets code touches the host. The
// render stats and lstats each listed file and never reads one; ReadFile is on
// the seam so a content read added later has one place to go through, and a fake
// can record every call and refuse it. The host's filesystem is osSecretFS
// (gt-gg9hv.66).
type SecretFS interface {
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
}

// SecretFile is one entry of deploy/daemon/secrets.yaml, whose header defines
// the fields once. Name is the file's name in the config directory, never a
// path.
type SecretFile struct {
	Name     string `yaml:"name"`
	Consumer string `yaml:"consumer"`
	Mode     string `yaml:"mode"`
	Required bool   `yaml:"required"`
}

// UnclaimedSecret is one entry of the manifest's unclaimed list, a file no
// daemon code or setting was found to read. It is recorded and never mounted.
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

// secretMode is the form of a manifest entry's mode: four octal digits with no
// group or other bits, since every file the manifest names is a credential or a
// script the daemon runs, and neither may be readable by another account.
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
	if err := validateSecretsManifest(m); err != nil {
		return SecretsManifest{}, err
	}
	return m, nil
}

// validateSecretsManifest checks every entry of a manifest, or of a spec's
// Secrets list, which the render does not trust to have been parsed by the
// command.
func validateSecretsManifest(m SecretsManifest) error {
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

// secretFS is the seam this spec touches the config directory through.
func (s ComposeSpec) secretFS() SecretFS {
	if s.SecretsFS != nil {
		return s.SecretsFS
	}
	return osSecretFS{}
}

// validateSecrets checks the secrets manifest entries a caller put on the spec.
// The entries are re-checked because a spec is a caller's value: the render does
// not trust that the command parsed them. Files are mounted from the config
// directory, so the spec must name one, and a BindRO directory that is or holds
// the config directory is refused, because it would mount the tree the manifest
// exists to replace.
func (s ComposeSpec) validateSecrets() error {
	if len(s.Secrets) == 0 {
		return nil
	}
	if s.Scratch {
		return fmt.Errorf("compose spec: Secrets is set on a scratch spec; only the live render carries the client configuration")
	}
	if s.ConfigDir == "" {
		return errors.New("compose spec: Secrets lists files in the config directory, but ConfigDir is empty; the manifest names files it cannot resolve")
	}
	if err := validateSecretsManifest(SecretsManifest{Files: s.Secrets}); err != nil {
		return fmt.Errorf("compose spec: %w", err)
	}
	// The directory itself is checked here so a missing one refuses the render
	// instead of reading every optional entry as absent and mounting nothing.
	info, err := s.secretFS().Stat(s.ConfigDir)
	if err != nil {
		return fmt.Errorf("compose spec: the config directory %q cannot be read, and a secrets manifest is set: %w", s.ConfigDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("compose spec: ConfigDir %q is not a directory, so the manifest's entries cannot be resolved", s.ConfigDir)
	}
	dir := filepath.Clean(s.ConfigDir)
	for _, p := range s.BindRO {
		if pathContains(dir, p) || pathContains(p, dir) {
			return fmt.Errorf("compose spec: BindRO %q is inside or contains the config directory %q; with a secrets manifest the render mounts the listed files, never the tree", p, s.ConfigDir)
		}
	}
	return nil
}

// secretMounts renders the daemon service's volumes for a spec's secrets
// manifest: one read-only bind of each listed file at its own path and one at
// the container's XDG_CONFIG_HOME path, sorted by name so a manifest reordered
// by hand renders the same file. An optional file that is not present is
// recorded as a comment and not mounted; a required one that is not present, or
// an entry that is a directory, a symlink, a special file or one whose mode
// exceeds the entry's maximum, is refused naming the entry.
//
// It lstats and stats each file through the spec's SecretFS and reads none of
// them: the render carries paths, never contents.
func secretMounts(s ComposeSpec) (string, error) {
	files := append([]SecretFile(nil), s.Secrets...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	secrets := s.secretFS()
	var b strings.Builder
	for _, f := range files {
		host := filepath.Join(s.ConfigDir, f.Name)
		// Lstat, not Stat alone: a link would be mounted as the file it names,
		// and the bind would then pin the target's inode, which is the file the
		// operator did not list.
		link, err := secrets.Lstat(host)
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist) && f.Required:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q is required, and %s does not exist", f.Name, host)
		case err != nil && errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(&b, "      # secrets manifest: optional file %q is not present, so it is not mounted\n", f.Name)
			continue
		case err != nil:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q cannot be read at %s: %w", f.Name, host, err)
		case link.Mode()&fs.ModeSymlink != 0:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q is a symlink at %s; the manifest mounts a regular file, never a link to one", f.Name, host)
		}
		info, err := secrets.Stat(host)
		switch {
		case err != nil:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q cannot be read at %s: %w", f.Name, host, err)
		case info.IsDir():
			return "", fmt.Errorf("compose spec: secrets manifest entry %q is the directory %s; the manifest mounts files, not directories", f.Name, host)
		case !info.Mode().IsRegular():
			return "", fmt.Errorf("compose spec: secrets manifest entry %q is %s, not a regular file; the manifest mounts files", f.Name, host)
		}
		if err := checkSecretMode(f, info.Mode().Perm()); err != nil {
			return "", err
		}
		b.WriteString(liveTokenMount(host, host))
		b.WriteString(liveTokenMount(host, filepath.Join(LiveConfigDirPath, f.Name)))
	}
	return b.String(), nil
}

// checkSecretMode refuses a file whose permission bits go beyond what the entry
// allows: any group or other bit, or any bit its maximum mode does not carry
// (gt-gg9hv.66). The message names the entry and the mode on disk, never the
// file's contents.
func checkSecretMode(f SecretFile, perm fs.FileMode) error {
	if perm&0o077 != 0 {
		return fmt.Errorf("compose spec: secrets manifest entry %q is mode %04o on disk; it is readable by group or other, and the manifest mounts it for one account", f.Name, perm)
	}
	// The mode is four octal digits, checked as such before either caller
	// reaches this (validateSecretsManifest), so the fold has no parse that can
	// fail.
	max := fs.FileMode(0)
	for _, digit := range f.Mode {
		max = max<<3 | fs.FileMode(digit-'0')
	}
	if beyond := perm &^ max; beyond != 0 {
		return fmt.Errorf("compose spec: secrets manifest entry %q is mode %04o on disk, which carries %04o beyond the manifest's maximum mode %s", f.Name, perm, beyond, f.Mode)
	}
	return nil
}
