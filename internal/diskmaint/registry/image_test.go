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
