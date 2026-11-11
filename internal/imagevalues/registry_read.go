package imagevalues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// tagDigestTimeout bounds one registry read.
	tagDigestTimeout = 30 * time.Second
	// tagDigestMaxBytes bounds one manifest answer, so a registry cannot make
	// the reader allocate without limit.
	tagDigestMaxBytes = 1 << 20
)

// tagDigestAccept asks for an image manifest first; an index is accepted so a
// tag that resolves to one still answers with its digest.
const tagDigestAccept = "application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json"

var (
	// imageNamePattern is one repository name under the owner, as the registry
	// accepts it.
	imageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// tagPattern is a tag the registry accepts; a tag reaches a URL path, so it
	// is checked before it is joined.
	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// CheckCredentialTransport refuses an http URL whose host is neither a loopback
// address nor plaintextHost, the one non-loopback host the caller names as the
// town's own Forgejo (forgejo.PlaintextHostOf). The viewer credential would
// otherwise cross a network in clear text to whoever answers there. It is the
// one rule for every read that credential makes — the registry manifest read
// and the Forgejo branch and compare reads alike. This package names no host of
// its own: the caller passes the town's.
func CheckCredentialTransport(u *url.URL, plaintextHost string) error {
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (isLoopbackHost(u.Host) || (plaintextHost != "" && strings.EqualFold(u.Host, plaintextHost))) {
		return nil
	}
	return fmt.Errorf("refusing to send the viewer credential over %s to %s: http is allowed only to a loopback address or the town's own Forgejo, every other host is read over https", u.Scheme, u.Host)
}

// isLoopbackHost reports whether host, with an optional port, is a loopback
// address this host answers on.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch strings.Trim(host, "[]") {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// SameEndpointRedirect is a client's redirect rule: a redirect that changes the
// scheme or the host is refused, so the credential attached to the first
// request never follows a Location to another endpoint.
func SameEndpointRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	prev := via[len(via)-1].URL
	if req.URL.Scheme != prev.Scheme || req.URL.Host != prev.Host {
		return fmt.Errorf("the endpoint redirected from %s://%s to %s://%s; a redirect that changes the scheme or the host is refused, so the credential stays with the first endpoint",
			prev.Scheme, prev.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// TagDigest reads the digest the registry holds for image at tag, through the
// viewer's basic credential. plaintextHost is the town's own Forgejo host the
// credential may reach over http, or "" for https only. The manifest bytes are
// hashed and must match the digest the registry reports, so a registry that
// answers another digest is an error, not a pair. A tag the registry does not
// hold is found=false with no error: an image not pushed yet is an answer, not a
// failure. The credential never reaches an error.
func TagDigest(ctx context.Context, hc *http.Client, reg Registry, plaintextHost, user, token, image, tag string) (digest string, found bool, err error) {
	if err := reg.Validate(); err != nil {
		return "", false, err
	}
	if !imageNamePattern.MatchString(image) {
		return "", false, fmt.Errorf("imagevalues: image name %q is not one repository name", image)
	}
	if !tagPattern.MatchString(tag) {
		return "", false, fmt.Errorf("imagevalues: tag %q is not a tag the registry accepts", tag)
	}
	// The URL is built from parts Validate and the patterns above have already
	// checked, so it is assembled rather than parsed: no part can make it fail.
	u := &url.URL{Scheme: reg.Scheme, Host: reg.Host, Path: "/v2/" + reg.Owner + "/" + image + "/manifests/" + tag}
	if err := CheckCredentialTransport(u, plaintextHost); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, tagDigestTimeout)
	defer cancel()
	req := (&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}).WithContext(ctx)
	req.Header.Set("Accept", tagDigestAccept)
	req.SetBasicAuth(user, token)
	client := &http.Client{CheckRedirect: SameEndpointRedirect}
	if hc != nil {
		c := *hc
		c.CheckRedirect = SameEndpointRedirect
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("asking the registry for %s:%s: %w", image, tag, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", false, fmt.Errorf("the registry answered HTTP %d for %s:%s", resp.StatusCode, image, tag)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, tagDigestMaxBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("reading the registry's answer for %s:%s: %w", image, tag, err)
	}
	if len(body) > tagDigestMaxBytes {
		return "", false, fmt.Errorf("the registry's answer for %s:%s is larger than %d bytes", image, tag, tagDigestMaxBytes)
	}
	answered := resp.Header.Get("Docker-Content-Digest")
	if !ValidDigest(answered) {
		return "", false, fmt.Errorf("the registry answered digest %q for %s:%s, not sha256:<64 lowercase hex>", answered, image, tag)
	}
	if got := manifestDigest(body); got != answered {
		return "", false, fmt.Errorf("the registry answered digest %s for %s:%s, but its manifest bytes hash to %s", answered, image, tag, got)
	}
	return answered, true, nil
}

// manifestDigest is the digest of body, the form a registry reports.
func manifestDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
