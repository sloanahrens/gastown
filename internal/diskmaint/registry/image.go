package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ImageInfo is what a pull-by-digest consumer needs to know about an image the
// registry holds: the digest of the manifest the tag names (the reference a pod
// pulls by) and the labels of the image config the platform's manifest points at.
type ImageInfo struct {
	// ManifestDigest is the sha256 of the manifest body ref names. For an image
	// index it is the index's own digest, which a pull resolves to its platform.
	ManifestDigest string
	// ConfigDigest is the digest of the platform image's config blob.
	ConfigDigest string
	// Labels are the config's labels.
	Labels map[string]string
}

// imageConfigDoc is the part of an image config blob the client reads.
type imageConfigDoc struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

// InspectImage reads the image ref names in repo for the given platform: the
// manifest, an index resolved to its os/arch entry, and the config blob that
// manifest names, whose labels it returns. Every body is checked against the
// digest that names it, so a registry cannot answer with bytes other than the
// ones the digests pin: the tag's manifest against Docker-Content-Digest, a
// child manifest and the config blob against the digest they were asked by. An
// index with no entry for the platform is an error, as is a config digest the
// blob does not hash to. A repo or ref that fails validation returns ErrBadName
// and sends no request.
func (c *Client) InspectImage(ctx context.Context, repo, ref, osName, arch string) (ImageInfo, error) {
	if !ValidRepo(repo) {
		return ImageInfo{}, fmt.Errorf("manifest %q: %w", repo, ErrBadName)
	}
	if !ValidTag(ref) && !ValidDigest(ref) {
		return ImageInfo{}, fmt.Errorf("manifest %q: %w", ref, ErrBadName)
	}
	walk := &manifestWalk{}
	path := "/v2/" + repo + "/manifests/" + ref
	header, body, err := c.getBody(ctx, path, manifestAccept, walk)
	if err != nil {
		return ImageInfo{}, err
	}
	top := hashOf(body)
	if header.Get("Docker-Content-Digest") != top {
		return ImageInfo{}, fmt.Errorf("GET %s: the Docker-Content-Digest does not name the body, which hashes to %s", path, top)
	}
	info := ImageInfo{ManifestDigest: top}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return ImageInfo{}, fmt.Errorf("GET %s: decode: %w", path, err)
	}
	if doc.isIndex() {
		child, err := ChildForPlatform(ref, body, osName, arch)
		if err != nil {
			return ImageInfo{}, fmt.Errorf("in %q: %w", repo, err)
		}
		childPath := "/v2/" + repo + "/manifests/" + child
		_, childBody, err := c.getBody(ctx, childPath, manifestAccept, walk)
		if err != nil {
			return ImageInfo{}, err
		}
		if got := hashOf(childBody); got != child {
			return ImageInfo{}, fmt.Errorf("GET %s: the body hashes to %s, not the digest asked", childPath, got)
		}
		doc = manifestDoc{}
		if err := json.Unmarshal(childBody, &doc); err != nil {
			return ImageInfo{}, fmt.Errorf("GET %s: decode: %w", childPath, err)
		}
		if doc.isIndex() {
			return ImageInfo{}, fmt.Errorf("GET %s: the %s/%s entry is an index, not an image", childPath, osName, arch)
		}
	}
	if !ValidDigest(doc.Config.Digest) {
		return ImageInfo{}, fmt.Errorf("manifest %s in %q names no config digest", ref, repo)
	}
	info.ConfigDigest = doc.Config.Digest
	blobPath := "/v2/" + repo + "/blobs/" + doc.Config.Digest
	_, blob, err := c.getBody(ctx, blobPath, "", walk)
	if err != nil {
		return ImageInfo{}, err
	}
	if got := hashOf(blob); got != doc.Config.Digest {
		return ImageInfo{}, fmt.Errorf("GET %s: the config blob hashes to %s, not the digest the manifest names", blobPath, got)
	}
	var cfg imageConfigDoc
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return ImageInfo{}, fmt.Errorf("GET %s: decode: %w", blobPath, err)
	}
	info.Labels = cfg.Config.Labels
	return info, nil
}

// ChildForPlatform is the digest of the one child an index body names for
// osName/arch. ref is the reference the body was read by and names the image in
// every refusal. An index with no child for the platform, or with two, is an
// error that lists every platform the index holds, so a caller refuses a tag it
// cannot resolve to one image rather than picking one. Every reader that
// resolves an index calls this function.
func ChildForPlatform(ref string, body []byte, osName, arch string) (string, error) {
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("the index at %s does not parse: %w", ref, err)
	}
	want := osName + "/" + arch
	var matches []string
	var seen []string
	for _, m := range doc.Manifests {
		platform := m.Platform.OS + "/" + m.Platform.Architecture
		if !slices.Contains(seen, platform) {
			seen = append(seen, platform)
		}
		if platform == want {
			matches = append(matches, m.Digest)
		}
	}
	held := "no platforms"
	if len(seen) > 0 {
		held = strings.Join(seen, ", ")
	}
	switch {
	case len(matches) == 0:
		return "", fmt.Errorf("the index at %s holds no %s entry; the platforms it holds are %s", ref, want, held)
	case len(matches) > 1:
		return "", fmt.Errorf("the index at %s holds %d %s entries; the platforms it holds are %s", ref, len(matches), want, held)
	}
	if !ValidDigest(matches[0]) {
		return "", fmt.Errorf("the index at %s names a %s child digest %q, not sha256:<64 lowercase hex>", ref, want, matches[0])
	}
	return matches[0], nil
}

// hashOf is the sha256 digest of body in the registry's form.
func hashOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
