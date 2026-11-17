package pincheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// TestImagePinDurabilityEveryLayerServed is the clean case: a two-platform
// index whose config and layers are all served reports nothing dead, and every
// one of them was asked for.
func TestImagePinDurabilityEveryLayerServed(t *testing.T) {
	t.Parallel()
	indexDigest := digestFor(0x01)
	amdChild, armChild := digestFor(0x11), digestFor(0x21)
	amdConfig, armConfig := digestFor(0x12), digestFor(0x22)
	amdLayers := []string{digestFor(0x13), digestFor(0x14)}
	armLayers := []string{digestFor(0x23), digestFor(0x24), digestFor(0x25)}

	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			indexDigest: {body: indexBody(mediaTypeOCIIndex,
				entry{digest: amdChild, platform: "linux/amd64"},
				entry{digest: armChild, platform: "linux/arm64"},
			), mediaType: mediaTypeOCIIndex},
			amdChild: {body: imageBody(mediaTypeOCIManifest, amdConfig, amdLayers...), mediaType: mediaTypeOCIManifest},
			armChild: {body: imageBody(mediaTypeOCIManifest, armConfig, armLayers...), mediaType: mediaTypeOCIManifest},
		},
		blobs: map[string]bool{amdConfig: true, armConfig: true},
	}
	for _, layer := range amdLayers {
		reg.blobs[layer] = true
	}
	for _, layer := range armLayers {
		reg.blobs[layer] = true
	}

	ref := "example.test/oci/docker@" + indexDigest
	res, err := Check(context.Background(), reg, ref, []string{"linux/amd64", "linux/arm64"})
	if err != nil {
		t.Fatalf("Check(%s) = error %v, want no error", ref, err)
	}
	if res.Ref != ref {
		t.Errorf("Result.Ref = %q, want the reference it was given, %q", res.Ref, ref)
	}
	if len(res.Dead) != 0 {
		t.Errorf("Result.Dead = %v, want none: every config and layer is served", res.Dead)
	}
	wantAsked := []string{amdConfig, amdLayers[0], amdLayers[1], armConfig, armLayers[0], armLayers[1], armLayers[2]}
	if got := reg.blobsAsked(); !slices.Equal(got, wantAsked) {
		t.Errorf("BlobExists was asked for %v, want each config and layer in manifest order %v", got, wantAsked)
	}
}

// TestImagePinDurabilityDeadLastLayer is the 2026-11-15 case (gt-mzoyya): the
// index and the first layers still answer, the last layer of arm64 does not.
func TestImagePinDurabilityDeadLastLayer(t *testing.T) {
	t.Parallel()
	indexDigest, child := digestFor(0x01), digestFor(0x11)
	config := digestFor(0x12)
	layers := []string{digestFor(0x13), digestFor(0x14), digestFor(0x15)}
	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			indexDigest: {body: indexBody(mediaTypeOCIIndex,
				entry{digest: child, platform: "linux/arm64"},
			), mediaType: mediaTypeOCIIndex},
			child: {body: imageBody(mediaTypeOCIManifest, config, layers...), mediaType: mediaTypeOCIManifest},
		},
		blobs: map[string]bool{config: true, layers[0]: true, layers[1]: true},
	}

	ref := "data.forgejo.org/oci/docker@" + indexDigest
	res, err := Check(context.Background(), reg, ref, []string{"linux/arm64"})
	if err != nil {
		t.Fatalf("Check(%s) = error %v, want no error", ref, err)
	}
	want := []string{layers[2] + " (linux/arm64 layer 3)"}
	if !slices.Equal(res.Dead, want) {
		t.Errorf("Result.Dead = %v, want the unserved last layer with its position %v", res.Dead, want)
	}
}

// TestImagePinDurabilityImageRef pins a manifest digest rather than an index:
// the manifest is the one image, checked for the one platform named.
func TestImagePinDurabilityImageRef(t *testing.T) {
	t.Parallel()
	imageDigest := digestFor(0x01)
	config, dead := digestFor(0x11), digestFor(0x12)
	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			imageDigest: {body: imageBody(mediaTypeOCIManifest, config, digestFor(0x13), dead), mediaType: mediaTypeOCIManifest},
		},
		blobs: map[string]bool{config: true, digestFor(0x13): true},
	}

	res, err := Check(context.Background(), reg, "example.test/oci/docker@"+imageDigest, []string{"linux/arm64"})
	if err != nil {
		t.Fatalf("Check = error %v, want no error", err)
	}
	want := []string{dead + " (linux/arm64 layer 2)"}
	if !slices.Equal(res.Dead, want) {
		t.Errorf("Result.Dead = %v, want %v", res.Dead, want)
	}
}

// TestImagePinDurabilityDeadConfigAndSharedLayer is the second half of the
// report: a config the registry dropped and a layer two platforms share appear
// once per position.
func TestImagePinDurabilityDeadConfigAndSharedLayer(t *testing.T) {
	t.Parallel()
	indexDigest := digestFor(0x01)
	amdChild, armChild := digestFor(0x11), digestFor(0x21)
	amdConfig, armConfig := digestFor(0x12), digestFor(0x22)
	shared := digestFor(0x31)
	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			indexDigest: {body: indexBody(mediaTypeOCIIndex,
				entry{digest: amdChild, platform: "linux/amd64"},
				entry{digest: armChild, platform: "linux/arm64"},
			), mediaType: mediaTypeOCIIndex},
			amdChild: {body: imageBody(mediaTypeOCIManifest, amdConfig, digestFor(0x13), shared), mediaType: mediaTypeOCIManifest},
			armChild: {body: imageBody(mediaTypeOCIManifest, armConfig, shared), mediaType: mediaTypeOCIManifest},
		},
		blobs: map[string]bool{amdConfig: true, digestFor(0x13): true},
	}

	res, err := Check(context.Background(), reg, "example.test/oci/docker@"+indexDigest, []string{"linux/amd64", "linux/arm64"})
	if err != nil {
		t.Fatalf("Check = error %v, want no error", err)
	}
	want := []string{
		shared + " (linux/amd64 layer 2)",
		armConfig + " (linux/arm64 config)",
		shared + " (linux/arm64 layer 1)",
	}
	if !slices.Equal(res.Dead, want) {
		t.Errorf("Result.Dead = %v, want each position the registry does not serve %v", res.Dead, want)
	}
}

// TestImagePinDurabilityManifestMediaTypes reads an index and its children in
// all four media types a registry answers with.
func TestImagePinDurabilityManifestMediaTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		indexType string
		imageType string
	}{
		{"oci index with oci manifest", mediaTypeOCIIndex, mediaTypeOCIManifest},
		{"oci index with v2 schema 2 manifest", mediaTypeOCIIndex, mediaTypeDockerManifest},
		{"v2 list with oci manifest", mediaTypeDockerList, mediaTypeOCIManifest},
		{"v2 list with v2 schema 2 manifest", mediaTypeDockerList, mediaTypeDockerManifest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			indexDigest, child := digestFor(0x01), digestFor(0x11)
			config, layer := digestFor(0x12), digestFor(0x13)
			reg := &fakeRegistry{
				manifests: map[string]fakeManifest{
					indexDigest: {body: indexBody(tc.indexType,
						entry{digest: child, platform: "linux/arm64"},
					), mediaType: tc.indexType},
					child: {body: imageBody(tc.imageType, config, layer), mediaType: tc.imageType},
				},
				blobs: map[string]bool{config: true, layer: true},
			}

			res, err := Check(context.Background(), reg, "example.test/oci/docker@"+indexDigest, []string{"linux/arm64"})
			if err != nil {
				t.Fatalf("Check = error %v, want the %s index and the %s manifest read", err, tc.indexType, tc.imageType)
			}
			if len(res.Dead) != 0 {
				t.Errorf("Result.Dead = %v, want none", res.Dead)
			}
		})
	}
}

// TestImagePinDurabilityRegistryMediaTypeDecides reads a manifest whose body
// declares no media type: the registry's answer decides, parameters and all.
func TestImagePinDurabilityRegistryMediaTypeDecides(t *testing.T) {
	t.Parallel()
	indexDigest, child := digestFor(0x01), digestFor(0x11)
	config, layer := digestFor(0x12), digestFor(0x13)
	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			indexDigest: {body: indexBody("",
				entry{digest: child, platform: "linux/arm64"},
			), mediaType: mediaTypeOCIIndex + "; charset=utf-8"},
			child: {body: imageBody("", config, layer), mediaType: mediaTypeOCIManifest + "; charset=utf-8"},
		},
		blobs: map[string]bool{config: true, layer: true},
	}

	res, err := Check(context.Background(), reg, "example.test/oci/docker@"+indexDigest, []string{"linux/arm64"})
	if err != nil {
		t.Fatalf("Check = error %v, want the registry's media type read without its parameters", err)
	}
	if len(res.Dead) != 0 {
		t.Errorf("Result.Dead = %v, want none", res.Dead)
	}
}

// TestImagePinDurabilityPlatformVariant checks the variant a platform names:
// only that variant's manifest is checked, and a dead layer there is its
// position.
func TestImagePinDurabilityPlatformVariant(t *testing.T) {
	t.Parallel()
	indexDigest := digestFor(0x01)
	v6Child, v7Child := digestFor(0x11), digestFor(0x21)
	v6Layer, v7Layer := digestFor(0x12), digestFor(0x22)
	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			indexDigest: {body: indexBody(mediaTypeOCIIndex,
				entry{digest: v6Child, platform: "linux/arm/v6"},
				entry{digest: v7Child, platform: "linux/arm/v7"},
			), mediaType: mediaTypeOCIIndex},
			v6Child: {body: imageBody(mediaTypeOCIManifest, digestFor(0x13), v6Layer), mediaType: mediaTypeOCIManifest},
			v7Child: {body: imageBody(mediaTypeOCIManifest, digestFor(0x23), v7Layer), mediaType: mediaTypeOCIManifest},
		},
		blobs: map[string]bool{digestFor(0x13): true, v6Layer: true, digestFor(0x23): true},
	}

	res, err := Check(context.Background(), reg, "example.test/oci/docker@"+indexDigest, []string{"linux/arm/v7"})
	if err != nil {
		t.Fatalf("Check = error %v, want no error", err)
	}
	want := []string{v7Layer + " (linux/arm/v7 layer 1)"}
	if !slices.Equal(res.Dead, want) {
		t.Errorf("Result.Dead = %v, want only the requested variant's dead layer %v", res.Dead, want)
	}
}

// TestImagePinDurabilityDuplicatePlatform names one platform twice: it is
// followed through its manifest once and its dead digests are reported once.
func TestImagePinDurabilityDuplicatePlatform(t *testing.T) {
	t.Parallel()
	t.Run("blobs asked once", func(t *testing.T) {
		t.Parallel()
		indexDigest, child := digestFor(0x01), digestFor(0x11)
		config, layer := digestFor(0x12), digestFor(0x13)
		reg := &fakeRegistry{
			manifests: map[string]fakeManifest{
				indexDigest: {body: indexBody(mediaTypeOCIIndex,
					entry{digest: child, platform: "linux/amd64"},
				), mediaType: mediaTypeOCIIndex},
				child: {body: imageBody(mediaTypeOCIManifest, config, layer), mediaType: mediaTypeOCIManifest},
			},
			blobs: map[string]bool{config: true, layer: true},
		}

		_, err := Check(context.Background(), reg, "example.test/oci/docker@"+indexDigest, []string{"linux/amd64", "linux/amd64"})
		if err != nil {
			t.Fatalf("Check = error %v, want no error", err)
		}
		wantAsked := []string{config, layer}
		if got := reg.blobsAsked(); !slices.Equal(got, wantAsked) {
			t.Errorf("BlobExists was asked for %v, want the platform named twice asked for once %v", got, wantAsked)
		}
	})
	t.Run("dead digest reported once", func(t *testing.T) {
		t.Parallel()
		indexDigest, child := digestFor(0x01), digestFor(0x11)
		config, layer := digestFor(0x12), digestFor(0x13)
		reg := &fakeRegistry{
			manifests: map[string]fakeManifest{
				indexDigest: {body: indexBody(mediaTypeOCIIndex,
					entry{digest: child, platform: "linux/amd64"},
				), mediaType: mediaTypeOCIIndex},
				child: {body: imageBody(mediaTypeOCIManifest, config, layer), mediaType: mediaTypeOCIManifest},
			},
			blobs: map[string]bool{layer: true},
		}

		res, err := Check(context.Background(), reg, "example.test/oci/docker@"+indexDigest, []string{"linux/amd64", "linux/amd64"})
		if err != nil {
			t.Fatalf("Check = error %v, want no error", err)
		}
		want := []string{config + " (linux/amd64 config)"}
		if !slices.Equal(res.Dead, want) {
			t.Errorf("Result.Dead = %v, want the platform named twice reported once %v", res.Dead, want)
		}
	})
	t.Run("image ref with the same name twice", func(t *testing.T) {
		t.Parallel()
		imageDigest := digestFor(0x01)
		config, layer := digestFor(0x12), digestFor(0x13)
		reg := &fakeRegistry{
			manifests: map[string]fakeManifest{
				imageDigest: {body: imageBody(mediaTypeOCIManifest, config, layer), mediaType: mediaTypeOCIManifest},
			},
			blobs: map[string]bool{config: true, layer: true},
		}

		res, err := Check(context.Background(), reg, "example.test/oci/docker@"+imageDigest, []string{"linux/arm64", "linux/arm64"})
		if err != nil {
			t.Fatalf("Check = error %v, want the one platform named twice checked", err)
		}
		if len(res.Dead) != 0 {
			t.Errorf("Result.Dead = %v, want none: every blob is served", res.Dead)
		}
	})
}

// TestImagePinDurabilityRefusals drives every refusal: what Check cannot
// decide is an error, never a dead digest and never a clean result.
func TestImagePinDurabilityRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ref       func(string) string
		platforms []string
		registry  func(*fakeRegistry) Registry
		want      string
	}{
		{
			name:      "unresolvable ref",
			ref:       func(string) string { return "example.test/oci/docker@" + digestFor(0xee) },
			platforms: []string{"linux/arm64"},
			want:      "reading the manifest at",
		},
		{
			name:      "platform missing from the index",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64", "linux/amd64"},
			want:      "the index names no linux/amd64 manifest",
		},
		{
			name:      "ref without a digest",
			ref:       func(string) string { return "example.test/oci/docker" },
			platforms: []string{"linux/arm64"},
			want:      "is not host/repo@sha256",
		},
		{
			name:      "ref without a registry host",
			ref:       func(string) string { return "oci/docker@" + digestFor(0x01) },
			platforms: []string{"linux/arm64"},
			want:      "names no registry host",
		},
		{
			name:      "ref with a short digest",
			ref:       func(string) string { return "example.test/oci/docker@sha256:abc" },
			platforms: []string{"linux/arm64"},
			want:      "is not host/repo@sha256",
		},
		{
			name:      "no platform named",
			ref:       func(ref string) string { return ref },
			platforms: nil,
			want:      "no platform was named",
		},
		{
			name:      "platform without an architecture",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux"},
			want:      `platform "linux" is not os/arch`,
		},
		{
			name:      "platform with an empty part",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux//arm64"},
			want:      `platform "linux//arm64" is not os/arch`,
		},
		{
			name:      "platform with more parts than os, arch and variant",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64/v8/extra"},
			want:      `platform "linux/arm64/v8/extra" is not os/arch`,
		},
		{
			name:      "index child manifest that does not resolve",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "reading the manifest at",
			registry: func(reg *fakeRegistry) Registry {
				delete(reg.manifests, digestFor(0x11))
				return reg
			},
		},
		{
			name:      "manifest read fails",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "the registry answered 503",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifestErr = errors.New("the registry answered 503")
				return reg
			},
		},
		{
			name:      "blob read fails",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "the registry answered 500",
			registry: func(reg *fakeRegistry) Registry {
				reg.blobErr = errors.New("the registry answered 500")
				return reg
			},
		},
		{
			name:      "manifest body does not parse",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "does not parse",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x01)] = fakeManifest{body: []byte("{"), mediaType: mediaTypeOCIIndex}
				return reg
			},
		},
		{
			name:      "media type outside the four",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "not one of the four manifest types",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x01)] = fakeManifest{body: []byte(`{"manifests":[]}`), mediaType: "text/plain"}
				return reg
			},
		},
		{
			name:      "index child that is not a digest",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "not sha256:<64 lowercase hex>",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x01)] = fakeManifest{body: indexBody(mediaTypeOCIIndex,
					entry{digest: "sha256:abc", platform: "linux/arm64"},
				), mediaType: mediaTypeOCIIndex}
				return reg
			},
		},
		{
			name:      "index child that is an index",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "is an index, not an image",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x11)] = fakeManifest{body: indexBody(mediaTypeOCIIndex,
					entry{digest: digestFor(0x12), platform: "linux/arm64"},
				), mediaType: mediaTypeOCIIndex}
				return reg
			},
		},
		{
			name:      "image ref asked for two platforms",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/amd64", "linux/arm64"},
			want:      "names one platform, not 2",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x01)] = fakeManifest{body: imageBody(mediaTypeOCIManifest, digestFor(0x12), digestFor(0x13)), mediaType: mediaTypeOCIManifest}
				return reg
			},
		},
		{
			name:      "index naming too many manifests for one platform",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "more than the 16",
			registry: func(reg *fakeRegistry) Registry {
				children := make([]entry, 0, maxPlatformManifests+1)
				for i := 0; i <= maxPlatformManifests; i++ {
					children = append(children, entry{digest: digestFor(byte(0x40 + i)), platform: "linux/arm64"})
				}
				reg.manifests[digestFor(0x01)] = fakeManifest{body: indexBody(mediaTypeOCIIndex, children...), mediaType: mediaTypeOCIIndex}
				return reg
			},
		},
		{
			name:      "config digest that is not a digest",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "names its config as",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x11)] = fakeManifest{body: imageBody(mediaTypeOCIManifest, "sha256:abc"), mediaType: mediaTypeOCIManifest}
				return reg
			},
		},
		{
			name:      "layer digest that is not a digest",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "names its layer 1 as",
			registry: func(reg *fakeRegistry) Registry {
				reg.manifests[digestFor(0x11)] = fakeManifest{body: imageBody(mediaTypeOCIManifest, digestFor(0x12), "sha256:abc"), mediaType: mediaTypeOCIManifest}
				return reg
			},
		},
		{
			name:      "no registry",
			ref:       func(ref string) string { return ref },
			platforms: []string{"linux/arm64"},
			want:      "no registry to ask",
			registry:  func(*fakeRegistry) Registry { return nil },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg, healthyRef := durabilityRegistry()
			var registry Registry = reg
			if tc.registry != nil {
				registry = tc.registry(reg)
			}
			res, err := Check(context.Background(), registry, tc.ref(healthyRef), tc.platforms)
			if err == nil {
				t.Fatalf("Check = %+v, nil, want an error naming the refusal", res)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check error = %q, want it to carry %q", err, tc.want)
			}
			if len(res.Dead) != 0 {
				t.Errorf("Result.Dead = %v, want none: a refusal is not a dead digest", res.Dead)
			}
		})
	}
}

// durabilityRegistry is a fake registry holding one linux/arm64 index whose
// every blob is served, and the reference that names it.
func durabilityRegistry() (*fakeRegistry, string) {
	indexDigest, child := digestFor(0x01), digestFor(0x11)
	config := digestFor(0x12)
	layer := digestFor(0x13)
	reg := &fakeRegistry{
		manifests: map[string]fakeManifest{
			indexDigest: {body: indexBody(mediaTypeOCIIndex,
				entry{digest: child, platform: "linux/arm64"},
			), mediaType: mediaTypeOCIIndex},
			child: {body: imageBody(mediaTypeOCIManifest, config, layer), mediaType: mediaTypeOCIManifest},
		},
		blobs: map[string]bool{config: true, layer: true},
	}
	return reg, "example.test/oci/docker@" + indexDigest
}

// digestFor is a well-formed digest whose hex digits are k repeated.
func digestFor(k byte) string {
	return "sha256:" + strings.Repeat(fmt.Sprintf("%02x", k), 32)
}

// entry is one child an index names: its digest and its platform, "os/arch" or
// "os/arch/variant".
type entry struct {
	digest   string
	platform string
}

// imageBody is the body a registry answers for an image manifest; an empty
// mediaType leaves the field out, as OCI allows.
func imageBody(mediaType, config string, layers ...string) []byte {
	declared := ""
	if mediaType != "" {
		declared = fmt.Sprintf(`"mediaType":%q,`, mediaType)
	}
	entries := make([]string, 0, len(layers))
	for _, layer := range layers {
		entries = append(entries, fmt.Sprintf(`{"digest":%q,"size":1}`, layer))
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":2,%s"config":{"digest":%q,"size":1},"layers":[%s]}`,
		declared, config, strings.Join(entries, ",")))
}

// indexBody is the body a registry answers for an index or a manifest list; an
// empty mediaType leaves the field out.
func indexBody(mediaType string, children ...entry) []byte {
	declared := ""
	if mediaType != "" {
		declared = fmt.Sprintf(`"mediaType":%q,`, mediaType)
	}
	manifests := make([]string, 0, len(children))
	for _, child := range children {
		parts := strings.Split(child.platform, "/")
		childPlatform := fmt.Sprintf(`{"os":%q,"architecture":%q`, parts[0], parts[1])
		if len(parts) == 3 {
			childPlatform += fmt.Sprintf(`,"variant":%q`, parts[2])
		}
		manifests = append(manifests, fmt.Sprintf(`{"digest":%q,"size":1,"platform":%s}`,
			child.digest, childPlatform+"}"))
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":2,%s"manifests":[%s]}`,
		declared, strings.Join(manifests, ",")))
}

// fakeManifest is one manifest a fakeRegistry answers.
type fakeManifest struct {
	body      []byte
	mediaType string
}

// fakeRegistry answers the manifests a test describes, serves the blobs it
// marks, and records every request so a test can see what was asked.
type fakeRegistry struct {
	manifests   map[string]fakeManifest
	blobs       map[string]bool
	manifestErr error
	blobErr     error
	asked       []string
}

func (f *fakeRegistry) Manifest(_ context.Context, repo, ref string) ([]byte, string, error) {
	f.asked = append(f.asked, "manifest "+repo+"@"+ref)
	if f.manifestErr != nil {
		return nil, "", f.manifestErr
	}
	m, ok := f.manifests[ref]
	if !ok {
		return nil, "", fmt.Errorf("no manifest at %s", ref)
	}
	return m.body, m.mediaType, nil
}

func (f *fakeRegistry) BlobExists(_ context.Context, _ string, digest string) (bool, error) {
	f.asked = append(f.asked, "blob "+digest)
	if f.blobErr != nil {
		return false, f.blobErr
	}
	return f.blobs[digest], nil
}

// blobsAsked is the digests BlobExists was asked for, in order.
func (f *fakeRegistry) blobsAsked() []string {
	var digests []string
	for _, call := range f.asked {
		if digest, ok := strings.CutPrefix(call, "blob "); ok {
			digests = append(digests, digest)
		}
	}
	return digests
}
