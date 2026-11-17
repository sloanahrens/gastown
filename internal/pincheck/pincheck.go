// Package pincheck decides whether every layer of a digest-pinned image is
// still served by its registry, so a pin that lost a layer upstream (gt-mzoyya)
// is refused before a build waits minutes on it (gt-f2jate).
package pincheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// The four manifest media types a registry answers with for an image or an
// index.
const (
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Registry is the registry API Check reads.
type Registry interface {
	// Manifest answers the manifest or index at ref in repo, which is
	// host/owner/name.
	Manifest(ctx context.Context, repo, ref string) (body []byte, mediaType string, err error)
	// BlobExists reports whether repo serves the blob named digest.
	BlobExists(ctx context.Context, repo, digest string) (bool, error)
}

// Result is the outcome of checking one pin.
type Result struct {
	// Ref is the image reference Check was given.
	Ref string
	// Dead names each digest the registry does not serve, as
	// "<digest> (<platform> config)" or "<digest> (<platform> layer <n>)", with
	// layer n one-based, so a digest that two platforms share appears once per
	// platform.
	Dead []string
}

// Check reports the digests of imageRef that the registry does not serve.
// imageRef is host/repo@sha256:<64 hex>; an index is followed to the manifests
// of platforms, each "os/arch" or "os/arch/variant". Anything Check cannot
// decide is an error, never Dead: a reference that does not resolve, a platform
// the index lacks, a manifest it cannot read and a registry error. A platform
// named more than once is asked for and reported once.
func Check(ctx context.Context, reg Registry, imageRef string, platforms []string) (Result, error) {
	if reg == nil {
		return Result{}, fmt.Errorf("pincheck: %s: no registry to ask", imageRef)
	}
	repo, ref, err := splitRef(imageRef)
	if err != nil {
		return Result{}, err
	}
	if len(platforms) == 0 {
		return Result{}, fmt.Errorf("pincheck: %s: no platform was named to check", imageRef)
	}
	requested := make([]platform, 0, len(platforms))
	named := make(map[platform]bool, len(platforms))
	for _, name := range platforms {
		p, err := parsePlatform(name)
		if err != nil {
			return Result{}, fmt.Errorf("pincheck: %s: %w", imageRef, err)
		}
		if named[p] {
			continue
		}
		named[p] = true
		requested = append(requested, p)
	}

	top, err := readManifest(ctx, reg, repo, ref)
	if err != nil {
		return Result{}, fmt.Errorf("pincheck: %s: %w", imageRef, err)
	}
	images, err := platformImages(ctx, reg, repo, imageRef, top, requested)
	if err != nil {
		return Result{}, err
	}
	res := Result{Ref: imageRef}
	for _, im := range images {
		if err := checkImage(ctx, reg, repo, im, &res); err != nil {
			return Result{}, fmt.Errorf("pincheck: %s: %w", imageRef, err)
		}
	}
	return res, nil
}

// platformImage is one platform's image manifest, ready for its blobs to be
// asked for.
type platformImage struct {
	platform platform
	doc      manifestDoc
}

// maxPlatformManifests bounds how many index entries one platform is followed
// through, so an index that names a great many children cannot decide how many
// requests the check sends. A platform's image is normally one entry.
const maxPlatformManifests = 16

// platformImages resolves top into the image manifest of every requested
// platform. An image ref is already that manifest for each platform; an index
// names one child per platform.
func platformImages(ctx context.Context, reg Registry, repo, imageRef string, top manifest, requested []platform) ([]platformImage, error) {
	if top.kind == kindImage {
		if len(requested) > 1 {
			return nil, fmt.Errorf("pincheck: %s: the reference is an image manifest, not an index, so it names one platform, not %d", imageRef, len(requested))
		}
		return []platformImage{{platform: requested[0], doc: top.doc}}, nil
	}
	var images []platformImage
	for _, p := range requested {
		children := indexChildren(top.doc, p)
		if len(children) == 0 {
			return nil, fmt.Errorf("pincheck: %s: the index names no %s manifest", imageRef, p)
		}
		if len(children) > maxPlatformManifests {
			return nil, fmt.Errorf("pincheck: %s: the index names %d %s manifests, more than the %d this check reads", imageRef, len(children), p, maxPlatformManifests)
		}
		for _, child := range children {
			if !imagevalues.ValidDigest(child) {
				return nil, fmt.Errorf("pincheck: %s: the index names %q for %s, not sha256:<64 lowercase hex>", imageRef, child, p)
			}
			childManifest, err := readManifest(ctx, reg, repo, child)
			if err != nil {
				return nil, fmt.Errorf("pincheck: %s: %s: %w", imageRef, p, err)
			}
			if childManifest.kind != kindImage {
				return nil, fmt.Errorf("pincheck: %s: the manifest %s the index names for %s is an index, not an image", imageRef, child, p)
			}
			images = append(images, platformImage{platform: p, doc: childManifest.doc})
		}
	}
	return images, nil
}

// checkImage asks for one platform's config and every layer, in the order the
// manifest names them, and records each position the registry does not serve.
func checkImage(ctx context.Context, reg Registry, repo string, im platformImage, res *Result) error {
	if err := blobServed(ctx, reg, repo, im.platform, "config", im.doc.Config.Digest, res); err != nil {
		return err
	}
	for i, layer := range im.doc.Layers {
		role := fmt.Sprintf("layer %d", i+1)
		if err := blobServed(ctx, reg, repo, im.platform, role, layer.Digest, res); err != nil {
			return err
		}
	}
	return nil
}

// blobServed records digest in res.Dead when the registry does not serve it. A
// digest that is not a digest is refused before it reaches the registry, whose
// client joins it to a request path.
func blobServed(ctx context.Context, reg Registry, repo string, p platform, role, digest string, res *Result) error {
	if !imagevalues.ValidDigest(digest) {
		return fmt.Errorf("the %s manifest names its %s as %q, not sha256:<64 lowercase hex>", p, role, digest)
	}
	served, err := reg.BlobExists(ctx, repo, digest)
	if err != nil {
		return fmt.Errorf("the %s %s %s: asking the registry: %w", p, role, digest, err)
	}
	if !served {
		res.Dead = append(res.Dead, fmt.Sprintf("%s (%s %s)", digest, p, role))
	}
	return nil
}

// splitRef splits imageRef into the repository the registry is asked for and
// the digest, refusing a reference whose first segment is not a registry host
// the registry client can address.
func splitRef(imageRef string) (repo, ref string, err error) {
	repo, ref, found := strings.Cut(imageRef, "@")
	if !found || !imagevalues.ValidRef(imageRef) {
		return "", "", fmt.Errorf("pincheck: %q is not host/repo@sha256:<64 lowercase hex>", imageRef)
	}
	host, name, hasRepo := strings.Cut(repo, "/")
	if !hasRepo || name == "" || (host != "localhost" && !strings.ContainsAny(host, ".:")) {
		return "", "", fmt.Errorf("pincheck: %q names no registry host", imageRef)
	}
	return repo, ref, nil
}

// platform is one requested platform: an os, an architecture and an optional
// variant.
type platform struct {
	os, arch, variant string
}

func (p platform) String() string {
	if p.variant == "" {
		return p.os + "/" + p.arch
	}
	return p.os + "/" + p.arch + "/" + p.variant
}

// parsePlatform reads "os/arch" or "os/arch/variant".
func parsePlatform(name string) (platform, error) {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" {
			return platform{}, fmt.Errorf("platform %q is not os/arch or os/arch/variant", name)
		}
	}
	switch len(parts) {
	case 2:
		return platform{os: parts[0], arch: parts[1]}, nil
	case 3:
		return platform{os: parts[0], arch: parts[1], variant: parts[2]}, nil
	}
	return platform{}, fmt.Errorf("platform %q is not os/arch or os/arch/variant", name)
}

// manifestDoc is the part of a manifest body Check reads. One shape serves an
// image (config and layers) and an index (manifests).
type manifestDoc struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

// mediaKind is what a manifest body describes.
type mediaKind int

const (
	kindUnknown mediaKind = iota
	kindImage
	kindIndex
)

// manifest is a manifest body and what it describes.
type manifest struct {
	doc  manifestDoc
	kind mediaKind
}

// readManifest reads the manifest at ref and classifies it. An unreadable body
// and a media type outside the four are errors: reading either as an image
// would report an unchecked image as served.
func readManifest(ctx context.Context, reg Registry, repo, ref string) (manifest, error) {
	body, returned, err := reg.Manifest(ctx, repo, ref)
	if err != nil {
		return manifest{}, fmt.Errorf("reading the manifest at %s: %w", ref, err)
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return manifest{}, fmt.Errorf("the manifest at %s does not parse: %w", ref, err)
	}
	switch mediaType := normalizedMediaType(doc.MediaType, returned); mediaType {
	case mediaTypeOCIIndex, mediaTypeDockerList:
		return manifest{doc: doc, kind: kindIndex}, nil
	case mediaTypeOCIManifest, mediaTypeDockerManifest:
		return manifest{doc: doc, kind: kindImage}, nil
	default:
		return manifest{}, fmt.Errorf("the manifest at %s has media type %q, not one of the four manifest types", ref, mediaType)
	}
}

// normalizedMediaType is the type a body is classified by: the type the
// document declares, else the type the registry answered with, without its
// parameters.
func normalizedMediaType(declared, returned string) string {
	mediaType := declared
	if strings.TrimSpace(mediaType) == "" {
		mediaType = returned
	}
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// indexChildren lists the digests of the index's entries for p, in the order
// the index names them. An entry matches when its os and architecture match,
// and its variant too when p names one.
func indexChildren(doc manifestDoc, p platform) []string {
	var children []string
	for _, entry := range doc.Manifests {
		entryPlatform := platform{os: entry.Platform.OS, arch: entry.Platform.Architecture, variant: entry.Platform.Variant}
		if entryPlatform.os != p.os || entryPlatform.arch != p.arch {
			continue
		}
		if p.variant != "" && entryPlatform.variant != p.variant {
			continue
		}
		children = append(children, entry.Digest)
	}
	return children
}
