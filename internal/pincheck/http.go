package pincheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// HTTP is the Registry that reads a registry over HTTP, including the
// anonymous Bearer handshake a public namespace of Forgejo or Docker Hub
// demands.
//
// The caller owns the network: Client supplies the transport, the cookie jar
// and the timeout, and a nil Client means http.DefaultClient. The redirect
// policy is set here rather than taken from the client, because the token it
// attaches must not follow a Location to another scheme or host.
type HTTP struct {
	Client *http.Client
}

// HTTP reads the same interface Check reads, so a caller passes it where a
// Registry is wanted.
var _ Registry = (*HTTP)(nil)

const (
	// httpScheme is the only scheme a registry is read over: the token a
	// challenge issues is a credential, so neither it nor the request carrying
	// it crosses a network in clear text.
	httpScheme = "https"
	// httpTimeout bounds one request when the injected client names no timeout
	// of its own, so a registry that never answers cannot hold a pin check open
	// past the caller's budget.
	httpTimeout = 30 * time.Second
	// httpMaxManifestBytes bounds one manifest body.
	httpMaxManifestBytes = 1 << 20
	// httpMaxTokenBytes bounds one token body.
	httpMaxTokenBytes = 64 << 10
)

// httpAccept asks for every media type readManifest classifies, so a registry
// answers the index or image manifest rather than a generic body.
const httpAccept = mediaTypeOCIIndex + ", " + mediaTypeDockerList + ", " +
	mediaTypeOCIManifest + ", " + mediaTypeDockerManifest

var (
	// registryHostPattern is a registry host and optional port: the first
	// segment of a repo, which reaches a URL host.
	registryHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)
	// repositoryPartPattern is one path component of a repository name, as a
	// distribution reference allows it.
	repositoryPartPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// tagPattern is a manifest ref a registry accepts, the tag half of a
	// digest-or-tag ref.
	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// bearerTokenPattern is the b64token an Authorization: Bearer header
	// carries, so a token holding a control character or a space is refused
	// before it reaches a header.
	bearerTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)
	// bearerSchemePattern finds the Bearer challenge in a WWW-Authenticate
	// value.
	bearerSchemePattern = regexp.MustCompile(`(?i)\bbearer\s+`)
)

// Manifest reads the manifest or index at ref in repo, asking the challenge's
// realm for an anonymous token when the registry answers 401.
func (h HTTP) Manifest(ctx context.Context, repo, ref string) ([]byte, string, error) {
	host, name, err := splitRegistryRepo(repo)
	if err != nil {
		return nil, "", err
	}
	if !imagevalues.ValidDigest(ref) && !tagPattern.MatchString(ref) {
		return nil, "", fmt.Errorf("pincheck: the manifest reference %q in %s is neither a digest nor a tag", ref, repo)
	}
	u := &url.URL{Scheme: httpScheme, Host: host, Path: "/v2/" + name + "/manifests/" + ref}
	resp, err := h.do(ctx, http.MethodGet, u, name, httpAccept)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("pincheck: the registry answered HTTP %d for the manifest at %s", resp.StatusCode, name)
	}
	body, err := readLimited(resp.Body, httpMaxManifestBytes)
	if err != nil {
		return nil, "", fmt.Errorf("pincheck: reading the manifest at %s: %w", name, err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// BlobExists reports whether repo serves digest, asking the challenge's realm
// for an anonymous token when the registry answers 401.
func (h HTTP) BlobExists(ctx context.Context, repo, digest string) (bool, error) {
	host, name, err := splitRegistryRepo(repo)
	if err != nil {
		return false, err
	}
	if !imagevalues.ValidDigest(digest) {
		return false, fmt.Errorf("pincheck: the blob digest %q in %s is not sha256:<64 lowercase hex>", digest, repo)
	}
	u := &url.URL{Scheme: httpScheme, Host: host, Path: "/v2/" + name + "/blobs/" + digest}
	resp, err := h.do(ctx, http.MethodHead, u, name, "")
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("pincheck: the registry answered HTTP %d for the blob %s in %s", resp.StatusCode, digest, name)
}

// do sends one request and answers its response. A 401 carrying a Bearer
// challenge costs one anonymous token request and one retry carrying the
// token; a second 401 is an error, so a registry that never accepts its own
// token cannot make this loop. name is the repository the token's scope is
// built from, never the scope the challenge names, so a challenge cannot ask
// for a token to another repository.
func (h HTTP) do(ctx context.Context, method string, u *url.URL, name, accept string) (*http.Response, error) {
	client := h.client()
	resp, err := h.send(ctx, client, method, u, accept, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge, err := bearerChallenge(resp.Header.Values("WWW-Authenticate"))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	token, err := h.token(ctx, client, challenge, name)
	if err != nil {
		return nil, err
	}
	retry, err := h.send(ctx, client, method, u, accept, token)
	if err != nil {
		return nil, err
	}
	if retry.StatusCode == http.StatusUnauthorized {
		_ = retry.Body.Close()
		return nil, fmt.Errorf("pincheck: the registry refused %s /v2/%s with the anonymous token it issued", method, name)
	}
	return retry, nil
}

// client is the client every request of one read runs on: the injected client,
// so the caller keeps its transport and its timeout, with a timeout of
// httpTimeout when it names none. The redirect policy is set here rather than
// taken from the caller, because a redirect to another scheme or host is the
// one way the token could leave the registry it was issued for.
func (h HTTP) client() *http.Client {
	c := http.Client{}
	if h.Client != nil {
		c = *h.Client
	}
	if c.Timeout == 0 {
		c.Timeout = httpTimeout
	}
	c.CheckRedirect = imagevalues.SameEndpointRedirect
	return &c
}

// send issues one request, with accept as its Accept header when it is not
// empty, and with the token as a Bearer credential only when one is passed:
// the token request itself is anonymous.
func (h HTTP) send(ctx context.Context, client *http.Client, method string, u *url.URL, accept, token string) (*http.Response, error) {
	// The URL is assembled from parts splitRegistryRepo and the callers have
	// checked, so it is built as a value rather than parsed: nothing here can
	// fail before the client sees the request.
	req := (&http.Request{Method: method, URL: u, Header: http.Header{}}).WithContext(ctx)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pincheck: the %s request to %s: %w", method, u.Host, err)
	}
	return resp, nil
}

// challenge is the realm and service a registry's 401 names: the token request
// is built from them and from nothing else.
type challenge struct {
	realm, service string
}

// bearerChallenge reads the realm and service of the first Bearer challenge in
// a 401's WWW-Authenticate values. A 401 naming no Bearer challenge, or a
// challenge naming no realm or no service, is refused rather than retried
// without a token: a token minted for the registry's default service is not a
// token for this registry.
func bearerChallenge(values []string) (challenge, error) {
	for _, value := range values {
		params, found := bearerParams(value)
		if !found {
			continue
		}
		realm, hasRealm := params["realm"]
		service, hasService := params["service"]
		if !hasRealm || realm == "" {
			return challenge{}, fmt.Errorf("pincheck: the registry's Bearer challenge names no realm")
		}
		if !hasService || service == "" {
			return challenge{}, fmt.Errorf("pincheck: the registry's Bearer challenge names no service")
		}
		return challenge{realm: realm, service: service}, nil
	}
	return challenge{}, fmt.Errorf("pincheck: the registry answered 401 without a Bearer challenge")
}

// bearerParams reads the auth-params after the Bearer scheme of one
// WWW-Authenticate value. A part without '=' ends the challenge, because the
// next scheme begins there.
func bearerParams(value string) (map[string]string, bool) {
	loc := bearerSchemePattern.FindStringIndex(value)
	if loc == nil {
		return nil, false
	}
	params := map[string]string{}
	for _, part := range splitAuthParams(value[loc[1]:]) {
		key, raw, ok := strings.Cut(part, "=")
		if !ok {
			break
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(raw), `"`)
	}
	return params, true
}

// splitAuthParams splits an auth-param list on the commas between params,
// keeping a comma inside a quoted value.
func splitAuthParams(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
			current.WriteRune(r)
		case r == '\\' && inQuotes:
			escaped = true
			current.WriteRune(r)
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if last := strings.TrimSpace(current.String()); last != "" {
		parts = append(parts, last)
	}
	return parts
}

// token asks the challenge's realm for an anonymous token scoped to name and
// answers it. The realm must be https, because the token would otherwise cross
// the network in clear text, and the token is never written to an error.
func (h HTTP) token(ctx context.Context, client *http.Client, ch challenge, name string) (string, error) {
	realm, err := url.Parse(ch.realm)
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("pincheck: the registry's Bearer challenge names the realm %q, not a URL", ch.realm)
	}
	if realm.Scheme != httpScheme {
		return "", fmt.Errorf("pincheck: the registry's Bearer challenge names the realm %q; a token is asked for over https only", ch.realm)
	}
	query := realm.Query()
	query.Set("service", ch.service)
	query.Set("scope", "repository:"+name+":pull")
	realm.RawQuery = query.Encode()
	resp, err := h.send(ctx, client, http.MethodGet, realm, "", "")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("pincheck: the token realm answered HTTP %d for the service %s", resp.StatusCode, ch.service)
	}
	body, err := readLimited(resp.Body, httpMaxTokenBytes)
	if err != nil {
		return "", fmt.Errorf("pincheck: reading the token for the service %s: %w", ch.service, err)
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("pincheck: the token realm's answer for the service %s does not parse", ch.service)
	}
	token := answer.Token
	if token == "" {
		token = answer.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("pincheck: the token realm's answer for the service %s carries no token", ch.service)
	}
	if !bearerTokenPattern.MatchString(token) {
		return "", fmt.Errorf("pincheck: the token realm's answer for the service %s is not a bearer token", ch.service)
	}
	return token, nil
}

// readLimited reads one body, refusing one larger than limit rather than
// reading whatever a registry sends.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("the body is larger than the %d bytes this read accepts", limit)
	}
	return body, nil
}

// splitRegistryRepo splits repo, host/owner/name, into the URL host and the
// repository name under /v2/. Both reach a request path, so a host that is
// neither localhost nor one carrying a dot or a port, and a name component
// that is not one lowercase path component, are refused before anything is
// sent.
func splitRegistryRepo(repo string) (host, name string, err error) {
	host, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" {
		return "", "", fmt.Errorf("pincheck: the repository %q is not host/owner/name", repo)
	}
	if !registryHostPattern.MatchString(host) || (host != "localhost" && !strings.ContainsAny(host, ".:")) {
		return "", "", fmt.Errorf("pincheck: the repository %q does not begin with a registry host:port", repo)
	}
	for _, part := range strings.Split(name, "/") {
		if !repositoryPartPattern.MatchString(part) {
			return "", "", fmt.Errorf("pincheck: the repository name %q is not lowercase path components", name)
		}
	}
	return host, name, nil
}
