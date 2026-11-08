package templates

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/manifestdata"
)

// SecretsRecreateNotice is printed with every manifest render. It is the one Go
// copy of the rotation line deploy/daemon/secrets.yaml's header carries; that
// header defines what rotating a listed secret costs (gt-gg9hv.66,
// gt-gg9hv.30).
const SecretsRecreateNotice = "rotating any listed secret requires recreating the daemon service (a bind mount of one file pins its inode)"

// SecretFS is the one seam through which the secrets code touches the host. The
// render lstats each listed file and never reads one; ReadFile is on the seam so
// a content read added later has one place to go through, and a fake can record
// every call and refuse it. The host's filesystem is osSecretFS (gt-gg9hv.66).
type SecretFS interface {
	Stat(name string) (fs.FileInfo, error)
	Lstat(name string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
}

// SecretFile is one entry of deploy/daemon/secrets.yaml.
type SecretFile = manifestdata.SecretFile

// UnclaimedSecret is one entry of the manifest's unclaimed list.
type UnclaimedSecret = manifestdata.UnclaimedSecret

// SecretsManifest is deploy/daemon/secrets.yaml parsed.
type SecretsManifest = manifestdata.SecretsManifest

// ParseSecretsManifest reads and validates a secrets manifest.
func ParseSecretsManifest(data []byte) (SecretsManifest, error) {
	return manifestdata.ParseSecretsManifest(data)
}

// validateSecretsManifest checks every entry of a manifest, or of a spec's
// Secrets list, which the render does not trust to have been parsed by the
// command.
func validateSecretsManifest(m SecretsManifest) error {
	return manifestdata.ValidateSecretsManifest(m)
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
// by hand renders the same file. The header of deploy/daemon/secrets.yaml
// defines what a required or optional entry and the mode maximum mean; this
// function refuses what the header says it refuses, naming the entry.
//
// Each entry is one Lstat, with no follow: an entry that is a symlink is refused
// rather than mounted as the file it names, and the mode checked is the mode of
// the name the manifest lists. The check happens here, at render time; the
// container engine makes the bind later, so a swap between the check and the
// mount is inherent to a file mount and is not something this render can rule
// out. The render carries paths, never contents.
func secretMounts(s ComposeSpec) (string, error) {
	files := append([]SecretFile(nil), s.Secrets...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	secrets := s.secretFS()
	var b strings.Builder
	for _, f := range files {
		host := filepath.Join(s.ConfigDir, f.Name)
		info, err := secrets.Lstat(host)
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist) && f.Required:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q is required, and %s does not exist", f.Name, host)
		case err != nil && errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(&b, "      # secrets manifest: optional file %q is not present, so it is not mounted\n", f.Name)
			continue
		case err != nil:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q cannot be read at %s: %w", f.Name, host, err)
		case info.Mode()&fs.ModeSymlink != 0:
			return "", fmt.Errorf("compose spec: secrets manifest entry %q is a symlink at %s; the manifest mounts a regular file, never a link to one", f.Name, host)
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

// checkSecretMode enforces the manifest entry's mode maximum. The header of
// deploy/daemon/secrets.yaml defines the maximum; this function compares the
// mode on disk to it and names the entry and that mode, never the file's
// contents (gt-gg9hv.66).
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
