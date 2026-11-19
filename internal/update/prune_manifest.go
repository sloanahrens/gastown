package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// pruneManifestMaxBytes bounds one manifest answer, so a registry cannot
	// make the bytes step allocate without limit.
	pruneManifestMaxBytes = 1 << 20
	// pruneManifestMaxChildren bounds how many platform manifests one index may
	// name; a larger index is refused, so its figure is left out.
	pruneManifestMaxChildren = 16
	// pruneManifestAccept asks for an image manifest or an index, as the digest
	// read does, so an index answers with its own digest.
	pruneManifestAccept = "application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json, " +
		"application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json"
)

// pruneRefPattern is a tag name as the registry takes it. A ref outside it, and
// outside the digest form, never reaches a URL path.
var pruneRefPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// PruneFigure is the bytes one image's delete would free. Known is set when
// every manifest the figure sums was read and summed; Bytes is then the figure.
// Otherwise the figure is left out: Unknown names, as image:tag, the image's
// deleted tags whose manifests could not be read, and Err is set when the
// layers read cannot be summed.
type PruneFigure struct {
	Image   string
	Bytes   int64
	Known   bool
	Unknown []string
	Err     error
}

// PruneFigures is the bytes step's answer for one plan: each image's figure, and
// the kept tags whose manifests could not be read. A kept tag's layers could be
// shared with any delete, so it leaves every image's figure out; it is named
// once here rather than under each image.
type PruneFigures struct {
	Images      map[string]PruneFigure
	KeptUnknown []string
}

// pruneManifestBody is the part of a manifest or index the bytes step reads:
// the layers an image manifest names, and the platform manifests an index names.
type pruneManifestBody struct {
	MediaType string `json:"mediaType"`
	Layers    []struct {
		Digest string `json:"digest"`
		Size   *int64 `json:"size"`
	} `json:"layers"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

// isIndex reports whether the body describes an index.
func (m pruneManifestBody) isIndex() bool {
	switch m.MediaType {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}
	return len(m.Manifests) > 0
}

// PruneBytes reads the manifest of every kept and deleted tag in plan, and
// returns each image's figure for its delete. The registry's blobs may be shared
// across images, so the layers of every kept tag, in any image, are never
// counted as freed. A manifest that cannot be read leaves the figure of every
// image it bears on out and names the tag; it never changes the plan. It
// returns an error only when the viewer credential cannot be read or the reader
// is not wired.
func (r PruneRegistryReader) PruneBytes(ctx context.Context, plan PrunePlan) (PruneFigures, error) {
	switch {
	case r.Doer == nil:
		return PruneFigures{}, errors.New("no HTTP client is wired for the prune bytes")
	case r.Credentials == nil:
		return PruneFigures{}, errors.New("no credential reader is wired for the prune bytes")
	case !pruneOwnerPattern.MatchString(r.Owner):
		return PruneFigures{}, errors.New("the prune bytes' owner is not one lowercase account name")
	}
	user, token, err := r.Credentials()
	if err != nil {
		return PruneFigures{}, fmt.Errorf("reading the viewer credential for the prune bytes: %w", err)
	}
	type read struct {
		layers []prunedLayer
		err    error
	}
	reads := map[string]read{}
	readEntry := func(e PruneEntry) {
		ref := e.Image + ":" + e.Tag
		if _, ok := reads[ref]; ok {
			return
		}
		layers, err := r.tagLayers(ctx, user, token, e.Image, e.Tag, e.Digest)
		reads[ref] = read{layers: layers, err: err}
	}
	for _, e := range plan.Keep {
		readEntry(e)
	}
	for _, e := range plan.Delete {
		readEntry(e)
	}

	kept := make([]prunedManifest, 0, len(plan.Keep))
	var keptUnknown []string
	for _, e := range plan.Keep {
		ref := e.Image + ":" + e.Tag
		if rd := reads[ref]; rd.err != nil {
			keptUnknown = append(keptUnknown, ref)
		} else {
			kept = append(kept, prunedManifest{Tag: ref, Layers: rd.layers})
		}
	}

	sort.Strings(keptUnknown)
	out := PruneFigures{Images: make(map[string]PruneFigure, len(pruneImages)), KeptUnknown: keptUnknown}
	for _, image := range pruneImages {
		f := PruneFigure{Image: image}
		var unknown []string
		var deleted []prunedManifest
		for _, e := range plan.Delete {
			if e.Image != image {
				continue
			}
			ref := e.Image + ":" + e.Tag
			if rd := reads[ref]; rd.err != nil {
				unknown = append(unknown, ref)
			} else {
				deleted = append(deleted, prunedManifest{Tag: ref, Layers: rd.layers})
			}
		}
		if len(unknown) > 0 || len(keptUnknown) > 0 {
			sort.Strings(unknown)
			f.Unknown = unknown
			out.Images[image] = f
			continue
		}
		total, err := uniqueBytes(deleted, kept)
		if err != nil {
			f.Err = err
			out.Images[image] = f
			continue
		}
		f.Bytes, f.Known = total, true
		out.Images[image] = f
	}
	return out, nil
}

// tagLayers reads the layers image:tag names. The tag must still point at
// digest, the digest the plan read, so the layers belong to the tag the plan
// lists. An index is read through each platform manifest it names, and the
// layers of every platform are returned together.
func (r PruneRegistryReader) tagLayers(ctx context.Context, user, token, image, tag, digest string) ([]prunedLayer, error) {
	answered, body, err := r.getManifest(ctx, user, token, image, tag)
	if err != nil {
		return nil, err
	}
	if answered != digest {
		return nil, fmt.Errorf("the registry now points %s:%s at %s, not the %s the plan read", image, tag, answered, digest)
	}
	var m pruneManifestBody
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("the manifest of %s:%s does not parse: %w", image, tag, err)
	}
	if !m.isIndex() {
		return manifestLayers(image, tag, m)
	}
	if len(m.Manifests) == 0 {
		return nil, fmt.Errorf("the index of %s:%s names no platform manifests", image, tag)
	}
	if len(m.Manifests) > pruneManifestMaxChildren {
		return nil, fmt.Errorf("the index of %s:%s names %d platform manifests, more than %d", image, tag, len(m.Manifests), pruneManifestMaxChildren)
	}
	var layers []prunedLayer
	for _, c := range m.Manifests {
		if !imagevalues.ValidDigest(c.Digest) {
			return nil, fmt.Errorf("the index of %s:%s names platform digest %q, not sha256:<64 lowercase hex>", image, tag, c.Digest)
		}
		answered, childBody, err := r.getManifest(ctx, user, token, image, c.Digest)
		if err != nil {
			return nil, err
		}
		if answered != c.Digest {
			return nil, fmt.Errorf("the registry answered digest %s for the platform manifest %s of %s", answered, c.Digest, image)
		}
		var child pruneManifestBody
		if err := json.Unmarshal(childBody, &child); err != nil {
			return nil, fmt.Errorf("the platform manifest %s of %s does not parse: %w", c.Digest, image, err)
		}
		if child.isIndex() {
			return nil, fmt.Errorf("the platform manifest %s of %s is an index, not an image", c.Digest, image)
		}
		part, err := manifestLayers(image, tag, child)
		if err != nil {
			return nil, err
		}
		layers = append(layers, part...)
	}
	return layers, nil
}

// manifestLayers returns the layers an image manifest names. A manifest that
// names no layers is unreadable, not a zero-byte figure: its layers cannot be
// checked against the delete. Each layer must name a digest and a size no
// negative. A layer with no size is carried with its size unknown, so the sum
// refuses it with its reason.
func manifestLayers(image, tag string, m pruneManifestBody) ([]prunedLayer, error) {
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("the manifest of %s:%s names no layers", image, tag)
	}
	layers := make([]prunedLayer, 0, len(m.Layers))
	for _, l := range m.Layers {
		if !imagevalues.ValidDigest(l.Digest) {
			return nil, fmt.Errorf("the manifest of %s:%s names layer digest %q, not sha256:<64 lowercase hex>", image, tag, l.Digest)
		}
		if l.Size != nil && *l.Size < 0 {
			return nil, fmt.Errorf("the manifest of %s:%s names layer %s with a negative size", image, tag, l.Digest)
		}
		layer := prunedLayer{Digest: l.Digest}
		if l.Size != nil {
			layer.Size, layer.SizeKnown = *l.Size, true
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// getManifest sends one GET for the manifest at ref, a tag or a digest, and
// returns the digest the registry answered, which must be the digest of the
// body. The body reaches no error: a refusal names the image and the ref only.
func (r PruneRegistryReader) getManifest(ctx context.Context, user, token, image, ref string) (string, []byte, error) {
	if !pruneRefPattern.MatchString(ref) && !imagevalues.ValidDigest(ref) {
		return "", nil, fmt.Errorf("the prune bytes' ref for %s is neither a tag nor a digest", image)
	}
	base, err := url.Parse(r.Base)
	if err != nil || base.Host == "" {
		return "", nil, errors.New("the Forgejo base URL for the prune bytes is not an absolute URL")
	}
	u := base.JoinPath("v2", r.Owner, image, "manifests", ref)
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	if err := imagevalues.CheckCredentialTransport(u, r.PlaintextHost); err != nil {
		return "", nil, err
	}
	// The request is assembled rather than parsed, as imagevalues does: its URL is
	// built from parts the checks above have already validated.
	req := (&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}).WithContext(ctx)
	req.Header.Set("Accept", pruneManifestAccept)
	req.SetBasicAuth(user, token)
	resp, err := r.Doer.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("asking the registry for %s:%s: %w", image, ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", nil, fmt.Errorf("the registry answered HTTP %d for %s:%s", resp.StatusCode, image, ref)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, pruneManifestMaxBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("reading the registry's manifest for %s:%s: %w", image, ref, err)
	}
	if len(body) > pruneManifestMaxBytes {
		return "", nil, fmt.Errorf("the registry's manifest for %s:%s is larger than %d bytes", image, ref, pruneManifestMaxBytes)
	}
	answered := resp.Header.Get("Docker-Content-Digest")
	if !imagevalues.ValidDigest(answered) {
		return "", nil, fmt.Errorf("the registry answered digest %q for %s:%s, not sha256:<64 lowercase hex>", answered, image, ref)
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != answered {
		return "", nil, fmt.Errorf("the registry answered digest %s for %s:%s, but its manifest bytes hash to %s", answered, image, ref, got)
	}
	return answered, body, nil
}
