package forgejocut

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// The modes the promote key's two halves carry: the private half is readable by
// its owner alone, and the public half is what GitHub is given.
const (
	promoteKeyMode fs.FileMode = 0o600
	promotePubMode fs.FileMode = 0o644
)

// EnsurePromoteKey is step 4's default-mode half (the script's
// ensure_promote_key): a rig with no promote key gets an ed25519 keypair, and a
// rig that has one keeps it, because a second cutover must not rotate a key
// GitHub already trusts. The private half is mode 0600 in a directory created
// mode 0700, and this process never reads it, prints it or logs it; the public
// half lands beside it as <key>.pub. A reused private half whose mode drifted is
// set back to 0600 before anything else touches it. A dry run prints the
// commands and runs nothing. Key files go through the run's Files seam, which
// records their modes; ssh-keygen runs through the exec seam.
//
// The run refuses a cutover with no Runner before this step, so ssh-keygen always
// has a seam to run through.
func (c *Cutover) EnsurePromoteKey() error {
	keys := c.files()
	o := c.Opts
	key, pub := o.PromoteKey, promotePubPath(o)
	fi, err := keys.Stat(key)
	switch {
	case err == nil:
		c.logf("reusing the promote key %s", key)
		if !o.DryRun {
			if err := enforceKeyMode(keys, key, fi); err != nil {
				return err
			}
		}
		return c.derivePromotePublicKey(keys, key, pub)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("could not look at the promote key %s: %w", key, err)
	}
	if o.DryRun {
		c.logf("+ mkdir -p %s (mode 700, when absent)", filepath.Dir(key))
		c.logf("+ ssh-keygen -q -t ed25519 -N '' -C promote-%s -f %s", o.Rig, key)
		c.logf("(dry run: no promote key was created)")
		return nil
	}
	if err := keys.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		return fmt.Errorf("could not make the promote key's directory %s: %w", filepath.Dir(key), err)
	}
	argv := []string{"ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "promote-" + o.Rig, "-f", key}
	if err := c.Runner.Run(argv); err != nil {
		// The child's own output reached the operator; err is its exit status.
		return fmt.Errorf("the promote key could not be created at %s: %w", key, err)
	}
	if err := keys.Chmod(key, promoteKeyMode); err != nil {
		return fmt.Errorf("could not set the promote key %s to mode 600: %w", key, err)
	}
	if err := keys.Chmod(pub, promotePubMode); err != nil {
		return fmt.Errorf("could not set the public half %s to mode 644: %w", pub, err)
	}
	c.logf("created the promote key %s (public half %s)", key, pub)
	return nil
}

// enforceKeyMode sets a reused private half back to mode 0600. fi is the mode the
// caller already read; the mode is changed only when it differs, so a second
// cutover over a converged key writes nothing.
func enforceKeyMode(keys Files, key string, fi fs.FileInfo) error {
	if fi.Mode().Perm() == promoteKeyMode {
		return nil
	}
	if err := keys.Chmod(key, promoteKeyMode); err != nil {
		return fmt.Errorf("could not set the promote key %s to mode 600: %w", key, err)
	}
	return nil
}

// derivePromotePublicKey is the existing-key half of step 4: a key whose public
// half is already there is reused as it is, and a key missing its public half has
// one derived from it rather than a new keypair minted, so GitHub keeps trusting
// the key it already has. The derived text is ssh-keygen's public half only: it is
// written to the sibling and never logged.
func (c *Cutover) derivePromotePublicKey(keys Files, key, pub string) error {
	if _, err := keys.Stat(pub); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not look at the public half %s: %w", pub, err)
	}
	if c.Opts.DryRun {
		c.logf("+ ssh-keygen -y -f %s > %s (dry run: not run)", key, pub)
		return nil
	}
	out, err := c.Runner.Output([]string{"ssh-keygen", "-y", "-f", key})
	if err != nil {
		// The output is not echoed: the private key is what ssh-keygen read.
		return fmt.Errorf("could not derive the public half of %s; remove it and re-run to mint a new keypair: %w", key, err)
	}
	tmp := pub + ".tmp"
	if err := keys.WriteFile(tmp, out, promotePubMode); err != nil {
		return fmt.Errorf("could not write the public half %s: %w", tmp, err)
	}
	if err := keys.Chmod(tmp, promotePubMode); err != nil {
		return fmt.Errorf("could not set the public half %s to mode 644: %w", tmp, err)
	}
	if err := keys.Rename(tmp, pub); err != nil {
		return fmt.Errorf("could not move the public half into %s (the text is left in %s): %w", pub, tmp, err)
	}
	c.logf("derived %s from the existing promote key", pub)
	return nil
}

// GhPromoteKeyCommands is step 4's note to the operator (the script's
// gh_promote_key_commands): the gh command that adds the key's PUBLIC half to
// GitHub with write access, so a green main verdict can advance GitHub main. Only
// the public half's path is named. Each word that a shell does not read literally
// is single-quoted, which the script did not do.
func (c *Cutover) GhPromoteKeyCommands() []string {
	o := c.Opts
	return []string{
		"",
		"Add the promote key's PUBLIC half to GitHub as a write deploy key:",
		"",
		"  gh repo deploy-key add " + shellWord(promotePubPath(o)) +
			" --repo " + shellWord(o.GitHubSlug) +
			" --title " + shellWord("promote-"+o.Rig) +
			" --allow-write",
	}
}

// promotePubPath is the public half's path: the private half's name with .pub
// appended, as the script derived it.
func promotePubPath(o *Options) string { return o.PromoteKey + ".pub" }

// shellWord renders one word of a printed shell command: the word unchanged when
// a shell reads every character of it literally, single-quoted otherwise, with
// each embedded quote written the way a shell escapes it.
func shellWord(word string) string {
	if word != "" && !strings.ContainsFunc(word, shellUnsafe) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// shellUnsafe reports whether r is a character a shell would read as syntax inside
// an unquoted word.
func shellUnsafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("._-/:@%+,=", r)
}
