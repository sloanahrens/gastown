package registry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// imageRegistry serves a repo's manifests and blobs by path from fixed bodies, the
// digest header each manifest names being the hash of its body.
func imageRegistry(manifests, blobs map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if body, ok := manifests[r.URL.Path]; ok {
			served(w, hashOf([]byte(body)), body)
			return
		}
		if body, ok := blobs[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}
}

func configWithLabel(value string) string {
	return fmt.Sprintf(`{"architecture":"arm64","os":"linux","config":{"Labels":{"org.gastown.gt-commit":%q}}}`, value)
}

func TestInspectImageReadsTheLabelThroughAnIndex(t *testing.T) {
	t.Parallel()
	cfg := configWithLabel("abc123")
	cfgDigest := hashOf([]byte(cfg))
	child := imageBody(cfgDigest, int64(len(cfg)))
	childDigest := hashOf([]byte(child))
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
		{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}},
		{"digest":%q,"platform":{"os":"linux","architecture":"arm64"}}]}`, digestOf('e'), childDigest)
	c := newTestClient(imageRegistry(
		map[string]string{"/v2/gastown-polecat/manifests/dev": index, "/v2/gastown-polecat/manifests/" + childDigest: child},
		map[string]string{"/v2/gastown-polecat/blobs/" + cfgDigest: cfg}))

	got, err := c.InspectImage(context.Background(), "gastown-polecat", "dev", "linux", "arm64")
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if got.ManifestDigest != hashOf([]byte(index)) {
		t.Errorf("ManifestDigest = %s, want the index's own digest %s", got.ManifestDigest, hashOf([]byte(index)))
	}
	if got.ConfigDigest != cfgDigest || got.Labels["org.gastown.gt-commit"] != "abc123" {
		t.Errorf("config digest %s, labels %v; want %s and the commit label", got.ConfigDigest, got.Labels, cfgDigest)
	}
}

func TestInspectImageReadsAPlainImageManifest(t *testing.T) {
	t.Parallel()
	cfg := configWithLabel("def456")
	cfgDigest := hashOf([]byte(cfg))
	manifest := imageBody(cfgDigest, int64(len(cfg)))
	c := newTestClient(imageRegistry(
		map[string]string{"/v2/gastown-polecat/manifests/dev": manifest},
		map[string]string{"/v2/gastown-polecat/blobs/" + cfgDigest: cfg}))
	got, err := c.InspectImage(context.Background(), "gastown-polecat", "dev", "linux", "arm64")
	if err != nil || got.Labels["org.gastown.gt-commit"] != "def456" || got.ManifestDigest != hashOf([]byte(manifest)) {
		t.Fatalf("InspectImage = %+v, %v", got, err)
	}
}

func TestInspectImageRefusals(t *testing.T) {
	t.Parallel()
	cfg := configWithLabel("abc123")
	cfgDigest := hashOf([]byte(cfg))
	image := imageBody(cfgDigest, int64(len(cfg)))
	noArm := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
		{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, digestOf('e'))
	cases := []struct {
		name      string
		manifests map[string]string
		blobs     map[string]string
		want      string
	}{
		{"an index with no linux/arm64 entry", map[string]string{"/v2/r/manifests/dev": noArm}, nil, "linux/arm64"},
		{"a config blob that does not hash to its digest", map[string]string{"/v2/r/manifests/dev": image}, map[string]string{"/v2/r/blobs/" + cfgDigest: configWithLabel("other")}, "not the digest the manifest names"},
		{"a missing config blob", map[string]string{"/v2/r/manifests/dev": image}, nil, "blobs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(imageRegistry(tc.manifests, tc.blobs))
			_, err := c.InspectImage(context.Background(), "r", "dev", "linux", "arm64")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("InspectImage error = %v, want one containing %q", err, tc.want)
			}
		})
	}
	t.Run("a child manifest that does not hash to its digest", func(t *testing.T) {
		t.Parallel()
		child := imageBody(cfgDigest, int64(len(cfg)))
		wrong := digestOf('9')
		index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
			{"digest":%q,"platform":{"os":"linux","architecture":"arm64"}}]}`, wrong)
		c := newTestClient(imageRegistry(map[string]string{"/v2/r/manifests/dev": index, "/v2/r/manifests/" + wrong: child}, nil))
		if _, err := c.InspectImage(context.Background(), "r", "dev", "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "not the digest asked") {
			t.Fatalf("error = %v, want the child hash refusal", err)
		}
	})
	t.Run("a bad repo sends no request", func(t *testing.T) {
		t.Parallel()
		h, n := countingHandler(imageRegistry(nil, nil))
		if _, err := newTestClient(h).InspectImage(context.Background(), "Bad Repo", "dev", "linux", "arm64"); err == nil || n.Load() != 0 {
			t.Fatalf("error = %v, requests = %d; want ErrBadName and none", err, n.Load())
		}
	})
}

// TestInspectImageFailurePaths drives each way a read can fail before it returns
// a label: a bad ref, a manifest or blob the registry will not serve, a header
// that does not name the body, and a body that is not the document it must be.
func TestInspectImageFailurePaths(t *testing.T) {
	t.Parallel()
	cfg := configWithLabel("abc123")
	cfgDigest := hashOf([]byte(cfg))
	image := imageBody(cfgDigest, int64(len(cfg)))
	archIndex := func(child string) string {
		return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":%q,"platform":{"os":"linux","architecture":"arm64"}}]}`, child)
	}
	notJSON := "not json"
	notJSONDigest := hashOf([]byte(notJSON))
	childIndexBody := archIndex(digestOf('f'))
	childIndexDigest := hashOf([]byte(childIndexBody))
	noConfig := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`
	badBlob := "not json either"
	badBlobDigest := hashOf([]byte(badBlob))
	cases := []struct {
		name string
		ref  string
		h    http.HandlerFunc
		want string
	}{
		{"a ref that fails validation", "bad ref!", imageRegistry(nil, nil), "failed validation"},
		{"a manifest the registry will not serve", "dev", imageRegistry(nil, nil), "manifests/dev"},
		{"a header that does not name the body", "dev", func(w http.ResponseWriter, r *http.Request) { served(w, digestOf('a'), image) }, "does not name the body"},
		{"a manifest that is not JSON", "dev", imageRegistry(map[string]string{"/v2/r/manifests/dev": notJSON}, nil), "decode"},
		{"a child manifest the registry will not serve", "dev", imageRegistry(map[string]string{"/v2/r/manifests/dev": archIndex(digestOf('d'))}, nil), "manifests/" + digestOf('d')},
		{"a child manifest that is not JSON", "dev", imageRegistry(map[string]string{"/v2/r/manifests/dev": archIndex(notJSONDigest), "/v2/r/manifests/" + notJSONDigest: notJSON}, nil), "decode"},
		{"a child that is itself an index", "dev", imageRegistry(map[string]string{"/v2/r/manifests/dev": archIndex(childIndexDigest), "/v2/r/manifests/" + childIndexDigest: childIndexBody}, nil), "is an index, not an image"},
		{"a manifest that names no config", "dev", imageRegistry(map[string]string{"/v2/r/manifests/dev": noConfig}, nil), "names no config digest"},
		{"a config blob that is not JSON", "dev", imageRegistry(map[string]string{"/v2/r/manifests/dev": imageBody(badBlobDigest, int64(len(badBlob)))}, map[string]string{"/v2/r/blobs/" + badBlobDigest: badBlob}), "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := newTestClient(tc.h).InspectImage(context.Background(), "r", tc.ref, "linux", "arm64")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("InspectImage error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
