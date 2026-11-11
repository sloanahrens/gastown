package registry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// copyTestManifest is a manifest body for the copy tests; its digest is what the
// registry names it by.
var copyTestManifest = []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1},"layers":[]}`)

// TestManifestBodyRoundTrip: a body PUT under a tag reads back as the same bytes,
// the same media type and the digest those bytes hash to.
func TestManifestBodyRoundTrip(t *testing.T) {
	t.Parallel()
	var stored []byte
	var storedType string
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPut:
			stored, _ = io.ReadAll(req.Body)
			storedType = req.Header.Get("Content-Type")
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			w.Header().Set("Content-Type", storedType)
			w.Header().Set("Docker-Content-Digest", hashOf(stored))
			_, _ = w.Write(stored)
		}
	})}}
	ctx := context.Background()

	if err := c.PutManifestBody(ctx, "gastown-polecat", "dev", "application/vnd.oci.image.manifest.v1+json", copyTestManifest); err != nil {
		t.Fatalf("PutManifestBody: %v", err)
	}
	got, err := c.ReadManifestBody(ctx, "gastown-polecat", "dev")
	if err != nil {
		t.Fatalf("ReadManifestBody: %v", err)
	}
	if string(got.Body) != string(copyTestManifest) || got.Digest != hashOf(copyTestManifest) || got.ContentType != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("read back %q (%s, %s), want the body PUT", got.Body, got.Digest, got.ContentType)
	}
}

// TestManifestBodyRefusesFailedPut: a PUT the registry answers with a failure is
// an error naming the status, never a stored tag.
func TestManifestBodyRefusesFailedPut(t *testing.T) {
	t.Parallel()
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}}

	err := c.PutManifestBody(context.Background(), "gastown-polecat", "dev", "application/vnd.oci.image.manifest.v1+json", copyTestManifest)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("PutManifestBody error = %v, want one naming HTTP 500", err)
	}
}

// TestManifestBodyRefusesDigestMismatch: a read whose body does not hash to the
// digest the registry names it by is refused rather than returned.
func TestManifestBodyRefusesDigestMismatch(t *testing.T) {
	t.Parallel()
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:"+"1111111111111111111111111111111111111111111111111111111111111111")
		_, _ = w.Write(copyTestManifest)
	})}}

	if _, err := c.ReadManifestBody(context.Background(), "gastown-polecat", "dev"); err == nil {
		t.Fatal("ReadManifestBody returned a body whose digest does not match, want an error")
	}
}

// TestManifestBodyRejectsBadNameWithoutRequest: a repo or tag that fails
// validation returns ErrBadName and sends nothing.
func TestManifestBodyRejectsBadNameWithoutRequest(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	})}}
	ctx := context.Background()

	if err := c.PutManifestBody(ctx, "gastown-polecat", "../dev", "application/json", copyTestManifest); !errors.Is(err, ErrBadName) {
		t.Fatalf("PutManifestBody bad tag error = %v, want ErrBadName", err)
	}
	if _, err := c.ReadManifestBody(ctx, "Gastown/Polecat", "dev"); !errors.Is(err, ErrBadName) {
		t.Fatalf("ReadManifestBody bad repo error = %v, want ErrBadName", err)
	}
	if _, err := c.ReadManifestBody(ctx, "gastown-polecat", "../dev"); !errors.Is(err, ErrBadName) {
		t.Fatalf("ReadManifestBody bad ref error = %v, want ErrBadName", err)
	}
	if err := c.PutManifestBody(ctx, "Gastown/Polecat", "dev", "application/json", copyTestManifest); !errors.Is(err, ErrBadName) {
		t.Fatalf("PutManifestBody bad repo error = %v, want ErrBadName", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d requests sent for names that fail validation, want none", n)
	}
}

// TestManifestBodyRefusesUnreachableRegistry: a request the transport cannot send,
// and a base URL that cannot form a request, are errors for both directions.
func TestManifestBodyRefusesUnreachableRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	down := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: failingTransport{err: errors.New("connection refused")}}}
	if _, err := down.ReadManifestBody(ctx, "gastown-polecat", "dev"); err == nil {
		t.Fatal("ReadManifestBody against an unreachable registry returned no error")
	}
	if err := down.PutManifestBody(ctx, "gastown-polecat", "dev", "application/json", copyTestManifest); err == nil {
		t.Fatal("PutManifestBody against an unreachable registry returned no error")
	}
	malformed := &Client{Base: "http://[bad"}
	if err := malformed.PutManifestBody(ctx, "gastown-polecat", "dev", "application/json", copyTestManifest); err == nil {
		t.Fatal("PutManifestBody with a base URL that forms no request returned no error")
	}
}
