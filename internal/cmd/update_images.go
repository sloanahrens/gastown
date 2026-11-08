package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// updateImagesDefaultOut is the deploy's values file, relative to where the
	// command runs.
	updateImagesDefaultOut = "deploy/values/images.yaml"
	// updateImagesTagLength is how much of the commit the images' tag carries.
	updateImagesTagLength = 12
	// updateImagesAccept asks for an image manifest first; an index is accepted
	// so the reader can refuse it by name instead of reporting a missing tag.
	updateImagesAccept = "application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json, " +
		"application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json"
	// updateImagesMaxManifest and updateImagesMaxConfig bound one registry
	// answer, so a registry cannot make the command allocate without limit.
	updateImagesMaxManifest = 1 << 20
	updateImagesMaxConfig   = 4 << 20
	// updateImagesTimeout bounds one registry request.
	updateImagesTimeout = 30 * time.Second
)

// updateImagesSeams are the subcommand's dependencies: the registry's address,
// the viewer credential, the HTTP client and the values-file write. A test
// supplies a fake registry and a counting write, so no test reaches the host.
type updateImagesSeams struct {
	RegistryBase string
	Credentials  func() (user, token string, err error)
	HTTPClient   *http.Client
	WriteValues  func(path string, v imagevalues.Values) error
}

// productionUpdateImagesSeams wires the real registry, viewer credential and
// values writer.
func productionUpdateImagesSeams() updateImagesSeams {
	return updateImagesSeams{
		RegistryBase: imagevalues.RegistryBase,
		Credentials:  viewerCredentialReader(forgejo.TokenPath),
		HTTPClient:   &http.Client{Timeout: updateImagesTimeout},
		WriteValues:  imagevalues.Write,
	}
}

// viewerCredentialReader reads the read-only viewer bot's login and token from
// its token file; neither value reaches an error. The token path lookup is
// passed in, so a test drives both refusals.
func viewerCredentialReader(tokenPath func(role string) (string, error)) func() (user, token string, err error) {
	return func() (string, string, error) {
		path, err := tokenPath(config.ForgejoRoleViewer)
		if err != nil {
			return "", "", err
		}
		return forgejo.ReadCredentialFile(path)
	}
}

// newUpdateImagesCmd builds `gt update images` over seams.
func newUpdateImagesCmd(seams updateImagesSeams) *cobra.Command {
	var sha, out string
	c := &cobra.Command{
		Use:   "images",
		Short: "Record the daemon and polecat image digests for one green commit",
		Long: `Record the daemon and polecat image digests for one green commit.

The command reads the registry's manifest and config blob for both images at
the tag <sha12> and writes them as a values file. It refuses a pair that does
not belong together: a missing tag, a registry error, a digest that is not
sha256:<64 lowercase hex>, or commit labels that differ from each other or from
--sha. A refusal writes no values file.

A second run for the same commit with the same digests changes nothing. A
recorded commit whose tag now answers another digest is refused, because an
image rebuilt under the same tag is a finding, not a silent replace.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateImages(cmd, seams, sha, out)
		},
	}
	c.Flags().StringVar(&sha, "sha", "", "The full green commit both images carry")
	c.Flags().StringVar(&out, "out", updateImagesDefaultOut, "The values file to write")
	return c
}

// runUpdateImages reads both images, holds their commits to sha, and records
// the pair once it is known good.
func runUpdateImages(cmd *cobra.Command, seams updateImagesSeams, sha, out string) error {
	if !imagevalues.ValidCommit(sha) {
		return fmt.Errorf("gt update images: --sha %q is not a full 40-character lowercase commit", sha)
	}
	if out == "" {
		return errors.New("gt update images: --out names no values file")
	}
	if err := validateUpdateImagesSeams(seams); err != nil {
		return err
	}
	user, token, err := seams.Credentials()
	if err != nil {
		return fmt.Errorf("gt update images: reading the viewer credential: %w", err)
	}
	reader := registryReader{
		base:  seams.RegistryBase,
		http:  seams.HTTPClient,
		user:  user,
		token: token,
	}
	tag := sha[:updateImagesTagLength]
	daemon, err := reader.image(cmd.Context(), "daemon", imagevalues.DaemonImage, tag)
	if err != nil {
		return fmt.Errorf("gt update images: %w", err)
	}
	polecat, err := reader.image(cmd.Context(), "polecat", imagevalues.PolecatImage, tag)
	if err != nil {
		return fmt.Errorf("gt update images: %w", err)
	}
	if daemon.Commit != polecat.Commit {
		return fmt.Errorf("gt update images: the daemon image %s carries commit %q, the polecat image %s carries commit %q; they must be one commit",
			daemon.Repo, daemon.Commit, polecat.Repo, polecat.Commit)
	}
	if daemon.Commit != sha {
		return fmt.Errorf("gt update images: the daemon image %s carries commit %q, not the requested %s", daemon.Repo, daemon.Commit, sha)
	}
	next := imagevalues.Values{
		Daemon:  imagevalues.Ref(imagevalues.DaemonImage, daemon.Digest),
		Polecat: imagevalues.Ref(imagevalues.PolecatImage, polecat.Digest),
		Commit:  sha,
	}
	return writeUpdateImagesValues(cmd, seams, out, next)
}

// refuseToReplaceARecordedPair answers nil when the file is absent or records
// another commit (so the caller writes), errNoValueWrite when it already
// records this pair (so the caller writes nothing), and an error for a file
// that cannot be read, a malformed file, or a rebuilt image under the same tag.
func refuseToReplaceARecordedPair(path string, next imagevalues.Values) error {
	existing, err := imagevalues.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gt update images: reading %s: %w", path, err)
	}
	if existing.Commit != next.Commit {
		return nil
	}
	if existing == next {
		return errNoValueWrite
	}
	return fmt.Errorf("gt update images: %s records daemon %s and polecat %s for commit %s, but the registry now answers %s and %s; an image rebuilt under the same tag is a finding, not a silent replace",
		path, existing.Daemon, existing.Polecat, next.Commit, next.Daemon, next.Polecat)
}

// errNoValueWrite says the values file already holds the pair, so the caller
// leaves it alone instead of rewriting it.
var errNoValueWrite = errors.New("the values file already holds the pair")

// writeUpdateImagesValues records the pair unless the file already holds it.
func writeUpdateImagesValues(cmd *cobra.Command, seams updateImagesSeams, path string, v imagevalues.Values) error {
	if err := refuseToReplaceARecordedPair(path, v); err != nil {
		if errors.Is(err, errNoValueWrite) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s already records %s; nothing changed\n", path, v.Commit)
			return nil
		}
		return err
	}
	if err := seams.WriteValues(path, v); err != nil {
		return fmt.Errorf("gt update images: writing %s: %w", path, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s for %s\n", path, v.Commit)
	return nil
}

// validateUpdateImagesSeams refuses a command that would report a read it
// never made.
func validateUpdateImagesSeams(s updateImagesSeams) error {
	switch {
	case s.RegistryBase == "":
		return errors.New("gt update images: no registry is wired")
	case s.Credentials == nil:
		return errors.New("gt update images: no credential reader is wired")
	case s.HTTPClient == nil:
		return errors.New("gt update images: no HTTP client is wired")
	case s.WriteValues == nil:
		return errors.New("gt update images: no values writer is wired")
	}
	return nil
}

// imageRead is one image as the registry answers it.
type imageRead struct {
	Repo   string
	Digest string
	Commit string
}

// registryReader reads manifests and config blobs from the registry's v2 API.
type registryReader struct {
	base  string
	http  *http.Client
	user  string
	token string
}

// registryManifest is the part of a manifest body the reader reads. It holds
// one shape for an image manifest and for an index, so an index is recognized
// and refused rather than read as an image with no config.
type registryManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []json.RawMessage `json:"manifests"`
}

// isIndex reports whether the body describes an index.
func (m registryManifest) isIndex() bool {
	switch m.MediaType {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}
	return len(m.Manifests) > 0
}

// registryConfig is the part of a config blob the reader reads.
type registryConfig struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

// errRegistryNotFound is the registry's 404: no such repository, tag or blob.
var errRegistryNotFound = errors.New("the registry answered HTTP 404")

// image reads name's digest and commit label at tag, naming the image in every
// refusal.
func (r registryReader) image(ctx context.Context, kind, name, tag string) (imageRead, error) {
	repo := imagevalues.Repo(name)
	manifestURL := strings.TrimRight(r.base, "/") + "/v2/" + imagevalues.RegistryOwner + "/" + name + "/manifests/" + tag
	var manifest registryManifest
	header, err := r.getJSON(ctx, manifestURL, updateImagesMaxManifest, &manifest)
	if errors.Is(err, errRegistryNotFound) {
		return imageRead{}, fmt.Errorf("the %s image %s has no tag %s in the registry", kind, repo, tag)
	}
	if err != nil {
		return imageRead{}, fmt.Errorf("reading the %s image %s: %w", kind, repo, err)
	}
	digest := header.Get("Docker-Content-Digest")
	if !imagevalues.ValidDigest(digest) {
		return imageRead{}, fmt.Errorf("the %s image %s answered digest %q, not sha256:<64 lowercase hex>", kind, repo, digest)
	}
	if manifest.isIndex() {
		return imageRead{}, fmt.Errorf("the %s image %s at tag %s is an index, not an image", kind, repo, tag)
	}
	if !imagevalues.ValidDigest(manifest.Config.Digest) {
		return imageRead{}, fmt.Errorf("the %s image %s names config digest %q, not sha256:<64 lowercase hex>", kind, repo, manifest.Config.Digest)
	}
	configURL := strings.TrimRight(r.base, "/") + "/v2/" + imagevalues.RegistryOwner + "/" + name + "/blobs/" + manifest.Config.Digest
	var config registryConfig
	if _, err := r.getJSON(ctx, configURL, updateImagesMaxConfig, &config); err != nil {
		return imageRead{}, fmt.Errorf("reading the %s image %s config: %w", kind, repo, err)
	}
	return imageRead{
		Repo:   repo,
		Digest: digest,
		Commit: config.Config.Labels[imagevalues.CommitLabel],
	}, nil
}

// getJSON sends one GET with the viewer's basic credential and decodes a JSON
// body into out, bounded by updateImagesTimeout and limit bytes.
func (r registryReader) getJSON(ctx context.Context, rawURL string, limit int64, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, updateImagesTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", updateImagesAccept)
	req.SetBasicAuth(r.user, r.token)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errRegistryNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("the registry answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading the registry's answer: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("the registry's answer is larger than %d bytes", limit)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("the registry's answer does not parse: %w", err)
	}
	return resp.Header, nil
}
