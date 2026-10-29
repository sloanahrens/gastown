package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
)

// bodyDigest is the digest a registry must name for body: the sha256 of its bytes.
func bodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestManifestDigestIsTheHashOfTheBodyRead(t *testing.T) {
	t.Parallel()
	body := imageBody(digestOf('c'), 1000)
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		served(w, bodyDigest(body), body)
	})

	got, err := c.ManifestDigest(context.Background(), "gastown-polecat", "dev")
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	if want := bodyDigest(body); got != want {
		t.Errorf("ManifestDigest = %q, want %q: the sha256 of the body", got, want)
	}
}

func TestManifestDigestRefusesAReplyThatDoesNotNameItsBody(t *testing.T) {
	t.Parallel()
	body := imageBody(digestOf('c'), 1000)
	cases := []struct{ name, header string }{
		{"the header names another digest", digestOf('a')},
		{"the header names no digest", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Docker-Content-Digest", tc.header)
				}
				_, _ = w.Write([]byte(body))
			})

			got, err := c.ManifestDigest(context.Background(), "gastown-polecat", "dev")
			if err == nil {
				t.Fatalf("ManifestDigest = %q, want a refusal: the header does not name the body", got)
			}
		})
	}
}

func TestManifestDigestMissingManifestIsErrNotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	_, err := c.ManifestDigest(context.Background(), "gastown-polecat", "dev")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ManifestDigest error = %v, want ErrNotFound", err)
	}
}

func TestManifestDigestRejectsBadRefBeforeRequest(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, repo, ref string }{
		{"bad repo", "../x", "dev"},
		{"traversal ref", "gastown-polecat", "../x"},
		{"empty ref", "gastown-polecat", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
			c := newTestClient(handler)

			_, err := c.ManifestDigest(context.Background(), tc.repo, tc.ref)
			if !errors.Is(err, ErrBadName) {
				t.Fatalf("ManifestDigest(%q, %q) error = %v, want ErrBadName", tc.repo, tc.ref, err)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("server saw %d requests, want 0", n)
			}
		})
	}
}
