package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// updateBdBaseImage is the base image the bd gateway image is built FROM:
	// the beads rig pushes it under this name, tagged with the commit.
	updateBdBaseImage = "beads-bd"
	// updateBdBasePinRelPath is the pin file, relative to the town's gastown
	// directory: the file Dockerfile.bd reads its FROM line from.
	updateBdBasePinRelPath = "deploy/bd/base-pin"
)

// updateBdBaseSeams are the subcommand's dependencies: the town's registry, the
// viewer credential, the registry tag read and the pin file. A test supplies
// fakes for each, so no test reaches the registry or writes the host's files.
type updateBdBaseSeams struct {
	// FindTownRoot finds the town whose Forgejo serves the registry.
	FindTownRoot func() (string, error)
	// Registry resolves where the town publishes its images.
	Registry    func(townRoot string) (imagevalues.Registry, error)
	Credentials func() (user, token string, err error)
	// TagDigest reads the digest the registry holds for image at tag. It is the
	// reader the daemon's pod pair check uses; found is false when the registry
	// holds no such tag.
	TagDigest func(ctx context.Context, reg imagevalues.Registry, user, token, image, tag string) (digest string, found bool, err error)
	// ReadPin returns the pin file's bytes; a missing file is fs.ErrNotExist.
	ReadPin func(path string) ([]byte, error)
	// WritePin replaces the pin file.
	WritePin func(path string, data []byte) error
}

// productionUpdateBdBaseSeams wires the real town, registry, viewer credential,
// registry reader and pin file.
func productionUpdateBdBaseSeams() updateBdBaseSeams {
	return updateBdBaseSeams{
		FindTownRoot: workspace.FindFromCwdOrError,
		Registry:     townImageRegistry,
		Credentials:  viewerCredentialReader(forgejo.TokenPath),
		TagDigest:    productionBdBaseTagDigest(&http.Client{Timeout: updateImagesTimeout}),
		ReadPin:      os.ReadFile,
		WritePin: func(path string, data []byte) error {
			return atomicfile.WriteFile(path, data, 0o644)
		},
	}
}

// productionBdBaseTagDigest reads the tag through imagevalues.TagDigest, the
// reader the pod pair check uses, with the town's plaintext host for the
// viewer credential.
func productionBdBaseTagDigest(hc *http.Client) func(context.Context, imagevalues.Registry, string, string, string, string) (string, bool, error) {
	return func(ctx context.Context, reg imagevalues.Registry, user, token, image, tag string) (string, bool, error) {
		return imagevalues.TagDigest(ctx, hc, reg, plaintextHostOf(reg), user, token, image, tag)
	}
}

// newUpdateBdBaseCmd builds `gt update bd-base` over seams.
func newUpdateBdBaseCmd(seams updateBdBaseSeams) *cobra.Command {
	var sha, pin string
	c := &cobra.Command{
		Use:   "bd-base",
		Short: "Pin the bd base image to the digest its commit tag resolves to",
		Long: `Pin the bd base image to the digest its commit tag resolves to.

The bd gateway image is built FROM the beads rig's base image, by digest, from
the pin file. After the beads CI pushes a new base, this command reads the
registry's answer for the base image at the tag <sha12> through the same reader
the daemon's pod pair check uses, and writes the digest reference to the pin
file. The next gastown image build picks the pin up.

The command refuses a tag the registry does not hold, an answer that is not a
sha256 digest, and a read that fails. A refusal writes nothing. A second run
for the same commit with the same digest leaves the pin file untouched. The
command prints the digest it wrote.

Without --pin the file is deploy/bd/base-pin under the town's gastown
directory. --pin writes the file it names instead.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateBdBase(cmd, seams, sha, pin)
		},
	}
	c.Flags().StringVar(&sha, "sha", "", "The full commit the beads base image was tagged with")
	c.Flags().StringVar(&pin, "pin", "", "The pin file to write; default is deploy/bd/base-pin under the town's gastown directory")
	return c
}

// runUpdateBdBase reads the base image's digest at the commit's tag and writes
// the pin when the digest differs from the one the file holds.
func runUpdateBdBase(cmd *cobra.Command, seams updateBdBaseSeams, sha, pin string) error {
	if !imagevalues.ValidCommit(sha) {
		return fmt.Errorf("gt update bd-base: --sha %q is not a full 40-character lowercase commit", sha)
	}
	if cmd.Flags().Changed("pin") && pin == "" {
		return errors.New("gt update bd-base: --pin names no pin file")
	}
	if err := validateUpdateBdBaseSeams(seams); err != nil {
		return err
	}
	townRoot, err := seams.FindTownRoot()
	if err != nil {
		return fmt.Errorf("gt update bd-base: finding the town root: %w", err)
	}
	if !cmd.Flags().Changed("pin") {
		pin = filepath.Join(townRoot, "gastown", updateBdBasePinRelPath)
	}
	registry, err := seams.Registry(townRoot)
	if err != nil {
		return fmt.Errorf("gt update bd-base: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return fmt.Errorf("gt update bd-base: %w", err)
	}
	user, token, err := seams.Credentials()
	if err != nil {
		return fmt.Errorf("gt update bd-base: reading the viewer credential: %w", err)
	}
	tag := sha[:updateImagesTagLength]
	digest, found, err := seams.TagDigest(cmd.Context(), registry, user, token, updateBdBaseImage, tag)
	if err != nil {
		return fmt.Errorf("gt update bd-base: %w", err)
	}
	repo := registry.Repo(updateBdBaseImage)
	if !found {
		return fmt.Errorf("gt update bd-base: the registry has no tag %s for %s; the beads rig pushes it after its base build lands", tag, repo)
	}
	if !imagevalues.ValidDigest(digest) {
		return fmt.Errorf("gt update bd-base: the registry answered %q for %s:%s, not sha256:<64 lowercase hex>", digest, repo, tag)
	}
	ref := []byte(registry.Ref(updateBdBaseImage, digest) + "\n")
	return writeUpdateBdBasePin(cmd, seams, pin, digest, ref)
}

// writeUpdateBdBasePin writes ref to pin unless the file already holds it. A
// file that cannot be read, other than one that does not exist, is a refusal:
// the command does not replace a pin it could not read.
func writeUpdateBdBasePin(cmd *cobra.Command, seams updateBdBaseSeams, pin, digest string, ref []byte) error {
	existing, err := seams.ReadPin(pin)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No pin yet: the write below creates it.
	case err != nil:
		return fmt.Errorf("gt update bd-base: reading %s: %w", pin, err)
	case bytes.Equal(existing, ref):
		fmt.Fprintf(cmd.OutOrStdout(), "%s already records %s; nothing changed\n", pin, digest)
		return nil
	}
	if err := seams.WritePin(pin, ref); err != nil {
		return fmt.Errorf("gt update bd-base: writing %s: %w", pin, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %s\n", pin, digest)
	return nil
}

// validateUpdateBdBaseSeams refuses a command that would report a read it never
// made.
func validateUpdateBdBaseSeams(s updateBdBaseSeams) error {
	switch {
	case s.FindTownRoot == nil:
		return errors.New("gt update bd-base: no town lookup is wired")
	case s.Registry == nil:
		return errors.New("gt update bd-base: no registry is wired")
	case s.Credentials == nil:
		return errors.New("gt update bd-base: no credential reader is wired")
	case s.TagDigest == nil:
		return errors.New("gt update bd-base: no tag reader is wired")
	case s.ReadPin == nil:
		return errors.New("gt update bd-base: no pin reader is wired")
	case s.WritePin == nil:
		return errors.New("gt update bd-base: no pin writer is wired")
	}
	return nil
}
