package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pruneManifestFake answers a manifest GET from manifests, keyed by image:ref,
// where ref is a tag or a digest. A ref it does not hold answers 404. Each
// answer names the digest of its body, as a registry does. It records every
// request's method.
type pruneManifestFake struct {
	manifests map[string][]byte
	// digests overrides the digest header a served ref answers with, keyed by
	// image:ref; an empty override answers no header.
	digests map[string]string
	// readErrs names the image:ref whose answer body fails to read.
	readErrs map[string]bool
	// doErr, when set, is the failure every request gets instead of an answer.
	doErr   error
	methods []string
}

// Do answers one manifest GET from the fake's map.
func (f *pruneManifestFake) Do(req *http.Request) (*http.Response, error) {
	f.methods = append(f.methods, req.Method)
	if f.doErr != nil {
		return nil, f.doErr
	}
	parts := strings.Split(req.URL.Path, "/")
	if len(parts) != 6 || parts[4] != "manifests" {
		return pruneManifestResponse(req, http.StatusNotFound, nil, ""), nil
	}
	key := parts[3] + ":" + parts[5]
	body, ok := f.manifests[key]
	if !ok {
		return pruneManifestResponse(req, http.StatusNotFound, nil, ""), nil
	}
	digest := pruneManifestDigest(body)
	if d, set := f.digests[key]; set {
		digest = d
	}
	resp := pruneManifestResponse(req, http.StatusOK, body, digest)
	if f.readErrs[key] {
		resp.Body = io.NopCloser(pruneFailingReader{err: errors.New("connection reset")})
	}
	return resp, nil
}

// pruneManifestResponse answers one request with code, body and the digest
// header, which is set only when digest is not empty.
func pruneManifestResponse(req *http.Request, code int, body []byte, digest string) *http.Response {
	h := http.Header{}
	if digest != "" {
		h.Set("Docker-Content-Digest", digest)
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: req}
}

// pruneManifestDigest is the digest of body, the form the registry reports.
func pruneManifestDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pruneSizedLayer is one layer an image manifest names, with its compressed size.
type pruneSizedLayer struct {
	digest string
	size   int64
}

// pruneImageBody is an image manifest that names layers.
func pruneImageBody(layers ...pruneSizedLayer) []byte {
	var parts []string
	for _, l := range layers {
		parts = append(parts, fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}`, l.digest, l.size))
	}
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[` + strings.Join(parts, ",") + `]}`)
}

// pruneIndexBody is an image index that names the platform manifests children.
func pruneIndexBody(children ...string) []byte {
	var parts []string
	for _, c := range children {
		parts = append(parts, fmt.Sprintf(`{"digest":%q}`, c))
	}
	return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + strings.Join(parts, ",") + `]}`)
}

// pruneServe makes the fake serve body as image:ref and returns the plan entry
// for that tag, whose digest is the body's.
func pruneServe(f *pruneManifestFake, image, tag string, body []byte) PruneEntry {
	f.manifests[image+":"+tag] = body
	return PruneEntry{Image: image, Tag: tag, Digest: pruneManifestDigest(body), Reason: "test"}
}

// pruneIndexOf serves child as an index's platform, at the digest its bytes
// hash to, and returns the plan entry for an index that names it.
func pruneIndexOf(f *pruneManifestFake, child []byte) PruneEntry {
	cd := pruneManifestDigest(child)
	f.manifests["gastown-daemon:"+cd] = child
	return pruneServe(f, "gastown-daemon", "d-old", pruneIndexBody(cd))
}

// TestPruneReadBytesLeavesFigureOutWhenTheRequestFails: a request that gets no
// answer, or an answer whose body cannot be read, leaves the figure out and
// names the tag.
func TestPruneReadBytesLeavesFigureOutWhenTheRequestFails(t *testing.T) {
	t.Parallel()
	const key = "gastown-daemon:d-old"
	cases := []struct {
		name  string
		setup func(f *pruneManifestFake)
	}{
		{"request gets no answer", func(f *pruneManifestFake) { f.doErr = errors.New("connection refused") }},
		{"answer body cannot be read", func(f *pruneManifestFake) { f.readErrs = map[string]bool{key: true} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &pruneManifestFake{manifests: map[string][]byte{}}
			entry := pruneServe(fake, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9}))
			tc.setup(fake)
			got := pruneBytesFor(t, fake, PrunePlan{Delete: []PruneEntry{entry}})
			assertPruneUnknown(t, got["gastown-daemon"], key)
		})
	}
}

// TestPruneReadBytesRefusesAnUnusableBaseOrTransport: a base URL that is not
// absolute, and a credential that would travel over plaintext to a host that is
// not the town's, are refused before any request is sent.
func TestPruneReadBytesRefusesAnUnusableBaseOrTransport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		base string
	}{
		{"base URL with no scheme", "forgejo.example"},
		{"plaintext to a host that is not the town's", "http://forgejo.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &pruneManifestFake{manifests: map[string][]byte{}}
			entry := pruneServe(fake, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9}))
			r := pruneBytesReader(fake)
			r.Base = tc.base
			got, err := r.PruneBytes(context.Background(), PrunePlan{Delete: []PruneEntry{entry}})
			if err != nil {
				t.Fatalf("PruneBytes() error = %v, want the tag named as unknown", err)
			}
			assertPruneUnknown(t, got["gastown-daemon"], "gastown-daemon:d-old")
			if len(fake.methods) != 0 {
				t.Errorf("requests = %v, want none for an unusable base or transport", fake.methods)
			}
		})
	}
}

// TestPruneReadBytesRefusesAnUnwiredReader: a reader with no HTTP client, no
// credential reader, or an owner that is not one account name returns an error
// and no figures.
func TestPruneReadBytesRefusesAnUnwiredReader(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	cases := []struct {
		name   string
		mutate func(r *PruneRegistryReader)
	}{
		{"no HTTP client", func(r *PruneRegistryReader) { r.Doer = nil }},
		{"no credential reader", func(r *PruneRegistryReader) { r.Credentials = nil }},
		{"owner that is not an account name", func(r *PruneRegistryReader) { r.Owner = "gastown/x" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := pruneBytesReader(fake)
			tc.mutate(&r)
			got, err := r.PruneBytes(context.Background(), PrunePlan{})
			if err == nil {
				t.Fatalf("PruneBytes() = %v, nil error; want a refusal", got)
			}
			if got != nil {
				t.Errorf("PruneBytes() returned figures %v with its error", got)
			}
		})
	}
}

// pruneBytesReader is the reader a PruneBytes test runs over fake.
func pruneBytesReader(fake PruneDoer) PruneRegistryReader {
	return PruneRegistryReader{
		Base:        "https://forgejo.example",
		Owner:       "gastown",
		Doer:        fake,
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
	}
}

// pruneBytesFor runs PruneBytes over plan and returns the figures, failing the
// test on a read error.
func pruneBytesFor(t *testing.T, fake PruneDoer, plan PrunePlan) map[string]PruneFigure {
	t.Helper()
	got, err := pruneBytesReader(fake).PruneBytes(context.Background(), plan)
	if err != nil {
		t.Fatalf("PruneBytes() error = %v", err)
	}
	return got
}

// TestPruneReadBytesCountsLayersHeldOnlyByTheDeletedTags: a layer a deleted tag
// shares with a kept tag, in any image, is not counted, since the registry's
// blobs are shared; a layer only deleted tags hold is counted once.
func TestPruneReadBytesCountsLayersHeldOnlyByTheDeletedTags(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	plan := PrunePlan{
		Keep: []PruneEntry{
			pruneServe(fake, "gastown-daemon", "d-new", pruneImageBody(pruneSizedLayer{pruneDigest("3"), 5e9})),
			pruneServe(fake, "gastown-polecat", "p-new", pruneImageBody(pruneSizedLayer{pruneDigest("1"), 1e9})),
		},
		Delete: []PruneEntry{
			pruneServe(fake, "gastown-daemon", "d-old", pruneImageBody(
				pruneSizedLayer{pruneDigest("1"), 1e9},
				pruneSizedLayer{pruneDigest("2"), 3e9},
			)),
		},
	}
	got := pruneBytesFor(t, fake, plan)
	if f := got["gastown-daemon"]; !f.Known || f.Bytes != 3e9 {
		t.Errorf("daemon figure = %+v, want known 3e9 (only layer 2 is held by no kept tag)", f)
	}
	if f := got["gastown-polecat"]; !f.Known || f.Bytes != 0 {
		t.Errorf("polecat figure = %+v, want known 0 with no delete", f)
	}
	for i, m := range fake.methods {
		if m != http.MethodGet {
			t.Errorf("request %d is %s, want only GET reads", i, m)
		}
	}
	if len(fake.methods) != 3 {
		t.Errorf("manifest reads = %d, want one per kept or deleted tag (3)", len(fake.methods))
	}
}

// TestPruneReadBytesSumsEveryPlatformOfAnIndex: a deleted tag that is an index
// frees the layers of every platform manifest it names.
func TestPruneReadBytesSumsEveryPlatformOfAnIndex(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	c1 := pruneImageBody(pruneSizedLayer{pruneDigest("4"), 4e8})
	c2 := pruneImageBody(pruneSizedLayer{pruneDigest("5"), 6e8})
	c1d, c2d := pruneManifestDigest(c1), pruneManifestDigest(c2)
	fake.manifests["gastown-daemon:"+c1d] = c1
	fake.manifests["gastown-daemon:"+c2d] = c2
	plan := PrunePlan{Delete: []PruneEntry{
		pruneServe(fake, "gastown-daemon", "d-old", pruneIndexBody(c1d, c2d)),
	}}
	got := pruneBytesFor(t, fake, plan)
	if f := got["gastown-daemon"]; !f.Known || f.Bytes != 1e9 {
		t.Errorf("daemon figure = %+v, want known 1e9 across both platforms", f)
	}
}

// TestPruneReadBytesLeavesFigureOutForAMovedTag: a tag the registry now points
// at another digest than the plan read has layers the plan does not list, so
// its figure is left out and the tag is named.
func TestPruneReadBytesLeavesFigureOutForAMovedTag(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	entry := pruneServe(fake, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9}))
	entry.Digest = pruneDigest("9")
	got := pruneBytesFor(t, fake, PrunePlan{Delete: []PruneEntry{entry}})
	assertPruneUnknown(t, got["gastown-daemon"], "gastown-daemon:d-old")
}

// TestPruneReadBytesLeavesFigureOutForAnUnreadableManifest: a manifest the
// bytes step cannot read leaves its image's figure out and names the tag, for
// each way a manifest can be unreadable.
func TestPruneReadBytesLeavesFigureOutForAnUnreadableManifest(t *testing.T) {
	t.Parallel()
	const key = "gastown-daemon:d-old"
	// oversized is a body one byte past the manifest size bound; it names no
	// layer, so only the size bound can refuse it.
	oversized := []byte(strings.Repeat(" ", pruneManifestMaxBytes+1))
	child := pruneImageBody(pruneSizedLayer{pruneDigest("4"), 4e8})
	nestedChild := pruneIndexBody()
	cases := []struct {
		name  string
		setup func(f *pruneManifestFake) PruneEntry
	}{
		{"manifest not found", func(*pruneManifestFake) PruneEntry {
			return PruneEntry{Image: "gastown-daemon", Tag: "d-old", Digest: pruneDigest("2"), Reason: "test"}
		}},
		{"manifest over the size bound", func(f *pruneManifestFake) PruneEntry {
			return pruneServe(f, "gastown-daemon", "d-old", oversized)
		}},
		{"body does not parse", func(f *pruneManifestFake) PruneEntry {
			return pruneServe(f, "gastown-daemon", "d-old", []byte("not a manifest"))
		}},
		{"layer with no size", func(f *pruneManifestFake) PruneEntry {
			return pruneServe(f, "gastown-daemon", "d-old", []byte(`{"layers":[{"digest":"`+pruneDigest("2")+`"}]}`))
		}},
		{"layer with a negative size", func(f *pruneManifestFake) PruneEntry {
			return pruneServe(f, "gastown-daemon", "d-old", []byte(`{"layers":[{"digest":"`+pruneDigest("2")+`","size":-1}]}`))
		}},
		{"layer digest not a digest", func(f *pruneManifestFake) PruneEntry {
			return pruneServe(f, "gastown-daemon", "d-old", []byte(`{"layers":[{"digest":"sha256:xyz","size":1}]}`))
		}},
		{"no digest header", func(f *pruneManifestFake) PruneEntry {
			e := pruneServe(f, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9}))
			f.digests = map[string]string{key: ""}
			return e
		}},
		{"digest header not the body's hash", func(f *pruneManifestFake) PruneEntry {
			// The plan's digest is the header's, so only the hash check refuses.
			e := pruneServe(f, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9}))
			f.digests = map[string]string{key: pruneDigest("8")}
			e.Digest = pruneDigest("8")
			return e
		}},
		{"index whose platform is an index", func(f *pruneManifestFake) PruneEntry {
			nd := pruneManifestDigest(nestedChild)
			f.manifests["gastown-daemon:"+nd] = nestedChild
			return pruneServe(f, "gastown-daemon", "d-old", pruneIndexBody(nd))
		}},
		{"index whose platform does not parse", func(f *pruneManifestFake) PruneEntry {
			return pruneIndexOf(f, []byte("not a manifest"))
		}},
		{"index whose platform has a layer with no size", func(f *pruneManifestFake) PruneEntry {
			return pruneIndexOf(f, []byte(`{"layers":[{"digest":"`+pruneDigest("2")+`"}]}`))
		}},
		{"index whose platform answers another digest", func(f *pruneManifestFake) PruneEntry {
			// The platform is served at a digest its own bytes do not hash to: its
			// header is the bytes' hash, which is not the digest the index names.
			cd := pruneDigest("6")
			f.manifests["gastown-daemon:"+cd] = pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9})
			return pruneServe(f, "gastown-daemon", "d-old", pruneIndexBody(cd))
		}},
		{"index whose platform is missing", func(f *pruneManifestFake) PruneEntry {
			return pruneServe(f, "gastown-daemon", "d-old", pruneIndexBody(pruneManifestDigest(child)))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &pruneManifestFake{manifests: map[string][]byte{}}
			entry := tc.setup(fake)
			got := pruneBytesFor(t, fake, PrunePlan{Delete: []PruneEntry{entry}})
			assertPruneUnknown(t, got["gastown-daemon"], key)
		})
	}
}

// TestPruneReadBytesRefusesARefThatIsNotATagOrDigest: a tag that would change the
// URL path (here, with a query character) is refused before the request, so the
// fake, which serves that exact ref, is never asked and the figure is left out.
func TestPruneReadBytesRefusesARefThatIsNotATagOrDigest(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	entry := pruneServe(fake, "gastown-daemon", "d-old?x", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9}))
	got := pruneBytesFor(t, fake, PrunePlan{Delete: []PruneEntry{entry}})
	assertPruneUnknown(t, got["gastown-daemon"], "gastown-daemon:d-old?x")
	if len(fake.methods) != 0 {
		t.Errorf("requests = %v, want none for a ref that is not a tag", fake.methods)
	}
}

// TestPruneReadBytesRefusesAnIndexBeforeFetchingItsPlatforms: an index that
// names too many platforms, or a platform digest that is not a digest, is
// refused from the index alone; no platform is requested.
func TestPruneReadBytesRefusesAnIndexBeforeFetchingItsPlatforms(t *testing.T) {
	t.Parallel()
	var children []string
	for i := 0; i <= pruneManifestMaxChildren; i++ {
		children = append(children, pruneDigest(fmt.Sprintf("%x", i%16)))
	}
	cases := map[string]string{
		"too many platforms":        string(pruneIndexBody(children...)),
		"malformed platform digest": string(pruneIndexBody("sha256:xyz")),
	}
	for name, index := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &pruneManifestFake{manifests: map[string][]byte{}}
			entry := pruneServe(fake, "gastown-daemon", "d-old", []byte(index))
			got := pruneBytesFor(t, fake, PrunePlan{Delete: []PruneEntry{entry}})
			assertPruneUnknown(t, got["gastown-daemon"], "gastown-daemon:d-old")
			if len(fake.methods) != 1 {
				t.Errorf("requests = %v, want only the index GET", fake.methods)
			}
		})
	}
}

// TestPruneReadBytesNamesAKeptTagInEveryFigure: a kept tag whose manifest cannot
// be read could hold any layer of a delete, so it leaves every image's figure
// out, and each image names it.
func TestPruneReadBytesNamesAKeptTagInEveryFigure(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	plan := PrunePlan{
		Keep: []PruneEntry{{Image: "gastown-polecat", Tag: "p-new", Digest: pruneDigest("7"), Reason: "test"}},
		Delete: []PruneEntry{
			pruneServe(fake, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("2"), 1e9})),
		},
	}
	got := pruneBytesFor(t, fake, plan)
	for _, image := range pruneImages {
		assertPruneUnknown(t, got[image], "gastown-polecat:p-new")
	}
	if u := got["gastown-daemon"].Unknown; len(u) != 1 || u[0] != "gastown-polecat:p-new" {
		t.Errorf("daemon unknown = %v, want only the kept tag, since the deleted tag reads", u)
	}
}

// TestPruneReadBytesRefusesALayerOfTwoSizes: one digest carried at two sizes
// cannot be summed, so the image's figure carries the refusal and no bytes.
func TestPruneReadBytesRefusesALayerOfTwoSizes(t *testing.T) {
	t.Parallel()
	fake := &pruneManifestFake{manifests: map[string][]byte{}}
	plan := PrunePlan{
		Keep: []PruneEntry{
			pruneServe(fake, "gastown-polecat", "p-new", pruneImageBody(pruneSizedLayer{pruneDigest("1"), 2e9})),
		},
		Delete: []PruneEntry{
			pruneServe(fake, "gastown-daemon", "d-old", pruneImageBody(pruneSizedLayer{pruneDigest("1"), 1e9})),
		},
	}
	f := pruneBytesFor(t, fake, plan)["gastown-daemon"]
	if !errors.Is(f.Err, ErrPruneRefused) {
		t.Fatalf("daemon figure error = %v, want ErrPruneRefused", f.Err)
	}
	if f.Known || f.Bytes != 0 {
		t.Errorf("daemon figure = %+v, want no figure with the refusal", f)
	}
}

// TestPruneReadBytesCredentialErrorIsAnError: a viewer credential that cannot be
// read stops the bytes step with an error and no figures.
func TestPruneReadBytesCredentialErrorIsAnError(t *testing.T) {
	t.Parallel()
	r := pruneBytesReader(&pruneManifestFake{manifests: map[string][]byte{}})
	r.Credentials = func() (string, string, error) { return "", "", errors.New("no token file") }
	got, err := r.PruneBytes(context.Background(), PrunePlan{})
	if err == nil {
		t.Fatalf("PruneBytes() = %v, nil error; want the credential error", got)
	}
	if got != nil {
		t.Errorf("PruneBytes() returned figures %v with its error", got)
	}
}

// assertPruneUnknown checks that f left its figure out and named ref.
func assertPruneUnknown(t *testing.T, f PruneFigure, ref string) {
	t.Helper()
	if f.Known {
		t.Fatalf("figure = %+v, want the figure left out", f)
	}
	if f.Err != nil {
		t.Fatalf("figure error = %v, want the tag named as unknown", f.Err)
	}
	for _, u := range f.Unknown {
		if u == ref {
			return
		}
	}
	t.Fatalf("unknown = %v, want it to name %s", f.Unknown, ref)
}
