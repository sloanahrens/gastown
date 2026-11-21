package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// prunePackagesLimit is the page size of one packages listing read.
	prunePackagesLimit = 50
	// prunePackagesMaxPages bounds how many pages one image's listing may take,
	// so a listing that never ends is an error rather than a hang.
	prunePackagesMaxPages = 200
	// prunePackagesMaxBytes bounds one packages page answer.
	prunePackagesMaxBytes = 4 << 20
	// pruneBranchLimit is the page size of one branch listing read.
	pruneBranchLimit = 100
	// pruneBranchMaxPages bounds how many pages one branch listing may take.
	pruneBranchMaxPages = 1000
	// pruneReasonReadBytes bounds how much of a refused answer's body is read
	// for its reason, so a huge body is never loaded to report a status.
	pruneReasonReadBytes = 512
	// pruneReasonMaxBytes caps the reason one refused answer reports.
	pruneReasonMaxBytes = 120
	// pruneReasonSideMaxBytes caps one answering-side header value.
	pruneReasonSideMaxBytes = 60
	// pruneTokenPrefixMin is the shortest prefix of the token a reason may not
	// end with, since a cut that leaves it may have dropped the rest.
	pruneTokenPrefixMin = 6
	// pruneNotShown is the reason shown in place of a cut through the token.
	pruneNotShown = "body not shown (credential-shaped)"
)

var (
	// pruneOwnerPattern is one account name, as the packages path takes it.
	pruneOwnerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// greenBranchPattern is a green ref: green/<sha12>, the sha12 a lowercase
	// hex prefix of the commit the image build was made from.
	greenBranchPattern = regexp.MustCompile(`^green/([0-9a-f]{12})$`)
)

// PruneLedger is the digest install's record, which the planner's installed and
// previous pairs are read from.
type PruneLedger interface {
	Read() (ImageRecord, error)
}

// ReadPrunePairs returns the installed pair and the previous pair the ledger
// holds. Both must name their daemon and polecat digests, and an installed pair
// with no previous recorded is an error, not an empty previous: the planner
// must never run on a record it cannot read in full. A bd digest may be empty,
// as ImagePair allows.
func ReadPrunePairs(ledger PruneLedger) (installed, previous ImagePair, err error) {
	if ledger == nil {
		return ImagePair{}, ImagePair{}, errors.New("no daemon image record is wired for the prune plan")
	}
	rec, err := ledger.Read()
	if err != nil {
		return ImagePair{}, ImagePair{}, fmt.Errorf("reading the daemon image record for the prune plan: %w", err)
	}
	switch {
	case rec.Current == (ImagePair{}):
		return ImagePair{}, ImagePair{}, errors.New("the daemon image record names no installed pair, so the prune plan cannot read one")
	case rec.Previous == (ImagePair{}):
		return ImagePair{}, ImagePair{}, errors.New("the daemon image record names an installed pair with no previous pair, so the prune plan cannot keep the pair a rollback restores")
	}
	for _, p := range []ImagePair{rec.Current, rec.Previous} {
		if err := validRecordedPair(p); err != nil {
			return ImagePair{}, ImagePair{}, fmt.Errorf("the daemon image record names a damaged pair: %w", err)
		}
	}
	return rec.Current, rec.Previous, nil
}

// PruneDoer sends one request and returns its answer. *http.Client satisfies it.
type PruneDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// PruneRegistryReader reads every tag of the three gastown images from the
// Forgejo packages API, with the viewer's read credential.
type PruneRegistryReader struct {
	// Base is the town's Forgejo root, such as https://forgejo.example.
	Base string
	// Owner is the account that owns the images.
	Owner string
	// PlaintextHost is the one non-loopback host the credential may reach over
	// http, as imagevalues.CheckCredentialTransport takes it; "" is https only.
	PlaintextHost string
	// Doer sends the requests. A *http.Client is given the endpoint's redirect
	// rule, as imagevalues.SameEndpointRedirect states it; any other Doer must
	// not follow a redirect off the first endpoint itself.
	Doer PruneDoer
	// Credentials returns the viewer's login and token. Neither reaches an error.
	Credentials func() (user, token string, err error)
	// Digest reads the digest the registry holds for image at tag, through the
	// viewer's credential. The packages listing carries no digest, so each tag's
	// digest comes from here. found is false when the registry holds no such
	// tag. A nil Digest is an error.
	Digest func(ctx context.Context, user, token, image, tag string) (digest string, found bool, err error)
}

// packageVersion is the part of one package version in a listing that the
// reader reads: its image name, its tag and its push time. The listing carries
// no digest.
type packageVersion struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// tag is the version as a PruneTag, its digest still to be read. A version the
// planner could not rank, match or name is an error here, which stops the
// listing read.
func (v packageVersion) tag(image string) (PruneTag, error) {
	switch {
	case v.Version == "":
		return PruneTag{}, fmt.Errorf("a %s package version has no tag", image)
	case v.CreatedAt.IsZero():
		return PruneTag{}, fmt.Errorf("the %s tag %q names no push time", image, v.Version)
	}
	return PruneTag{Name: v.Version, PushedAt: v.CreatedAt}, nil
}

// Images reads the tags of each of the three gastown images, following every
// page of each listing until it is complete. An image with no tags is an
// answer, an empty listing whose total is zero; a listing that cannot be read
// in full is an error, and no tags are returned with it.
func (r PruneRegistryReader) Images(ctx context.Context) (map[string][]PruneTag, error) {
	switch {
	case r.Doer == nil:
		return nil, errors.New("no HTTP client is wired for the prune listing")
	case r.Credentials == nil:
		return nil, errors.New("no credential reader is wired for the prune listing")
	case r.Digest == nil:
		return nil, errors.New("no digest reader is wired for the prune listing")
	case !pruneOwnerPattern.MatchString(r.Owner):
		return nil, errors.New("the prune listing's owner is not one lowercase account name")
	}
	user, token, err := r.Credentials()
	if err != nil {
		return nil, fmt.Errorf("reading the viewer credential for the prune listing: %w", err)
	}
	out := make(map[string][]PruneTag, len(pruneImages))
	for _, image := range pruneImages {
		tags, err := r.imageTags(ctx, user, token, image)
		if err != nil {
			return nil, err
		}
		out[image] = tags
	}
	return out, nil
}

// imageTags reads every page of image's listing. The total every page reports
// must not change while the pages are read. The walk ends at an empty page or
// once the total is read; a listing that ends short of its total is an error.
// A version named by a digest is a platform manifest under an image index, not
// a tag: it counts toward the total and is not returned. Each tag's digest is
// then read from the registry; one that cannot be read fails the whole listing.
func (r PruneRegistryReader) imageTags(ctx context.Context, user, token, image string) ([]PruneTag, error) {
	var tags []PruneTag
	total, seen := -1, 0
	for page := 1; ; page++ {
		if page > prunePackagesMaxPages {
			return nil, fmt.Errorf("the Forgejo packages listing for %s runs past %d pages", image, prunePackagesMaxPages)
		}
		items, pageTotal, err := r.page(ctx, user, token, image, page)
		if err != nil {
			return nil, err
		}
		if total < 0 {
			total = pageTotal
		} else if pageTotal != total {
			return nil, fmt.Errorf("the Forgejo packages listing for %s changed while it was read: its total went from %d to %d", image, total, pageTotal)
		}
		if len(items) == 0 {
			break
		}
		seen += len(items)
		for _, v := range items {
			if v.Name == "" {
				return nil, fmt.Errorf("the Forgejo packages listing for %s holds a package with no name", image)
			}
			if v.Name != image || strings.HasPrefix(v.Version, "sha256:") {
				continue
			}
			t, err := v.tag(image)
			if err != nil {
				return nil, err
			}
			tags = append(tags, t)
		}
		if seen >= total {
			break
		}
	}
	switch {
	case seen < total:
		return nil, fmt.Errorf("the Forgejo packages listing for %s is truncated: %d of %d packages read", image, seen, total)
	case seen > total:
		return nil, fmt.Errorf("the Forgejo packages listing for %s changed while it was read: %d packages read against a total of %d", image, seen, total)
	}
	for i := range tags {
		digest, found, err := r.Digest(ctx, user, token, image, tags[i].Name)
		switch {
		case err != nil:
			return nil, fmt.Errorf("reading the %s tag %q's digest from the registry: %w", image, tags[i].Name, err)
		case !found:
			return nil, fmt.Errorf("the registry holds no digest for the %s tag %q, so the plan cannot read every tag", image, tags[i].Name)
		case !imagevalues.ValidDigest(digest):
			return nil, fmt.Errorf("the registry's digest for the %s tag %q is not sha256:<64 lowercase hex>", image, tags[i].Name)
		}
		tags[i].Digest = digest
	}
	return tags, nil
}

// endpointDoer is the Doer a listing page is sent through. A *http.Client is
// copied with imagevalues.SameEndpointRedirect ahead of its own redirect rule,
// so a redirect that leaves the first endpoint is refused before the next
// request carries the credential there. Any other Doer cannot be changed, so
// page checks the endpoint its answer came from instead.
func (r PruneRegistryReader) endpointDoer() PruneDoer {
	c, ok := r.Doer.(*http.Client)
	if !ok {
		return r.Doer
	}
	cc := *c
	own := cc.CheckRedirect
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := imagevalues.SameEndpointRedirect(req, via); err != nil {
			return err
		}
		if own != nil {
			return own(req, via)
		}
		return nil
	}
	return &cc
}

// page reads one page of image's packages and the total the answer reports. A
// refused answer's body reaches an error only as its reason, through
// pruneRefusal, which redacts the viewer's credential and bounds the text.
func (r PruneRegistryReader) page(ctx context.Context, user, token, image string, page int) ([]packageVersion, int, error) {
	u, err := r.listingURL(image, page)
	if err != nil {
		return nil, 0, err
	}
	if err := imagevalues.CheckCredentialTransport(u, r.PlaintextHost); err != nil {
		return nil, 0, err
	}
	req := (&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}).WithContext(ctx)
	req.SetBasicAuth(user, token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.endpointDoer().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("asking the Forgejo packages API for %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// An answer that names no request, or names another endpoint, is refused: the
	// credential may have been sent off the endpoint to get it.
	if answered := resp.Request; answered == nil || answered.URL == nil || answered.URL.Scheme != u.Scheme || answered.URL.Host != u.Host {
		return nil, 0, fmt.Errorf("the Forgejo packages API for %s did not answer from the endpoint it was asked, so its answer is refused", image)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, pruneRefusal(resp, u, user, token, image)
	}
	total, err := strconv.Atoi(resp.Header.Get("X-Total-Count"))
	if err != nil || total < 0 {
		return nil, 0, fmt.Errorf("the Forgejo packages answer for %s names no total, so its listing cannot be shown complete", image)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, prunePackagesMaxBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("reading the Forgejo packages answer for %s: %w", image, err)
	}
	if len(body) > prunePackagesMaxBytes {
		return nil, 0, fmt.Errorf("the Forgejo packages answer for %s is larger than %d bytes", image, prunePackagesMaxBytes)
	}
	var items []packageVersion
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, 0, fmt.Errorf("the Forgejo packages answer for %s is not a package listing", image)
	}
	return items, total, nil
}

// pruneRefusal is the error for a non-200 packages answer: it names the status
// and image, then Forgejo's own reason, then the request and who answered, in a
// bracket after the reason.
func pruneRefusal(resp *http.Response, u *url.URL, user, token, image string) error {
	msg := fmt.Sprintf("the Forgejo packages API answered HTTP %d for %s", resp.StatusCode, image)
	body, err := io.ReadAll(io.LimitReader(resp.Body, pruneReasonReadBytes))
	reason := "the answer body could not be read"
	if err == nil {
		reason = pruneAnswerReason(body, user, token)
	}
	if reason != "" {
		msg += ": " + reason
	}
	return errors.New(msg + " [" + pruneRequestSide(resp, u, user, token) + "]")
}

// pruneRequestSide names the request a refused answer answered and who answered
// it: the path and query as sent, which carry no credential, then the Server and
// Via header values. Each header value is capped at pruneReasonSideMaxBytes, and
// an absent one is left out.
func pruneRequestSide(resp *http.Response, u *url.URL, user, token string) string {
	parts := []string{"GET " + u.EscapedPath() + "?" + u.RawQuery}
	for _, h := range []struct{ header, label string }{
		{"Server", "server"},
		{"Via", "via"},
	} {
		if v := pruneCredentialSafe(resp.Header.Get(h.header), pruneReasonSideMaxBytes, user, token); v != "" {
			parts = append(parts, h.label+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// pruneAnswerReason is a refused answer's own reason, or "" when the answer
// gives none. The text is cut and redacted by pruneCredentialSafe.
func pruneAnswerReason(body []byte, user, token string) string {
	text, ok := pruneReasonText(body)
	if !ok {
		return ""
	}
	return pruneCredentialSafe(text, pruneReasonMaxBytes, user, token)
}

// pruneReasonText is the text a reason is taken from. A body that is not JSON
// is its own text. A body that looks like JSON (its first non-space byte is '{'
// or '[') is JSON even when it is not valid, which it is not when the reason
// read cut it at the read bound: it gives no text, never its raw bytes, since a
// long error body can carry the credential. A valid JSON body gives text only
// when it is an object whose "message" is a string; any other JSON gives none.
func pruneReasonText(body []byte) (string, bool) {
	if !json.Valid(body) {
		if t := strings.TrimSpace(string(body)); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			return "", false
		}
		return string(body), true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", false
	}
	var msg string
	if err := json.Unmarshal(obj["message"], &msg); err != nil {
		return "", false
	}
	return msg, true
}

// pruneCredentialSafe is text as a refused answer shows it: its first line,
// printable ASCII only (every other byte becomes a space), cut to limit bytes,
// then the viewer's user and token replaced with "***". The cut comes first, so
// a token the cut splits is not replaced; a text that then ends with a prefix of
// the token is not shown at all.
func pruneCredentialSafe(text string, limit int, user, token string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	b := []byte(text)
	for i, c := range b {
		if c < ' ' || c > '~' {
			b[i] = ' '
		}
	}
	if len(b) > limit {
		b = b[:limit]
	}
	text = string(b)
	for _, secret := range []string{user, token} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	text = strings.TrimSpace(text)
	if pruneEndsInToken(text, token) {
		return pruneNotShown
	}
	return text
}

// pruneEndsInToken reports whether text ends with a prefix of token of
// pruneTokenPrefixMin or more characters, short of the whole token.
func pruneEndsInToken(text, token string) bool {
	for n := len(token) - 1; n >= pruneTokenPrefixMin; n-- {
		if strings.HasSuffix(text, token[:n]) {
			return true
		}
	}
	return false
}

// listingURL is the packages listing of the owner's container packages named
// image, at page. The image name and page reach the query only as encoded
// values.
func (r PruneRegistryReader) listingURL(image string, page int) (*url.URL, error) {
	base, err := url.Parse(r.Base)
	if err != nil || base.Host == "" {
		return nil, errors.New("the Forgejo base URL for the prune listing is not an absolute URL")
	}
	u := base.JoinPath("api", "v1", "packages", r.Owner)
	// JoinPath drops the leading slash when the base has no path, and a Go
	// server answers such a request line 400 before its router sees it
	// (gt-4nxkck).
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	u.RawQuery = url.Values{
		"type":  {"container"},
		"q":     {image},
		"page":  {strconv.Itoa(page)},
		"limit": {strconv.Itoa(prunePackagesLimit)},
	}.Encode()
	return u, nil
}

// BranchLister lists one page of a repository's branches. *forgejo.Client
// satisfies it.
type BranchLister interface {
	ListBranches(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.Branch, error)
}

// ReadGreenRefs returns the sha12 of every green/<sha12> branch of owner/repo,
// following the branch pages until one comes back empty. A page is not taken as
// the last for coming back short: a server may answer fewer branches than the
// page size asks for. A branch named any other way is not a green ref and is
// not in the set. A listing that fails on any page is an error with no set
// returned, since a set missing its refs would let the planner delete a green
// image.
func ReadGreenRefs(ctx context.Context, branches BranchLister, owner, repo string) (map[string]bool, error) {
	if branches == nil {
		return nil, errors.New("no branch listing is wired for the green refs")
	}
	green := map[string]bool{}
	for page := 1; ; page++ {
		if page > pruneBranchMaxPages {
			return nil, fmt.Errorf("the branch listing of %s/%s runs past %d pages", owner, repo, pruneBranchMaxPages)
		}
		list, err := branches.ListBranches(ctx, owner, repo, page, pruneBranchLimit)
		if err != nil {
			// A Forgejo answer's text can carry the response body, so only its
			// status is named.
			var apiErr *forgejo.APIError
			if errors.As(err, &apiErr) {
				return nil, fmt.Errorf("the Forgejo branch listing for %s/%s answered HTTP %d", owner, repo, apiErr.StatusCode)
			}
			return nil, fmt.Errorf("the Forgejo branch listing for %s/%s failed", owner, repo)
		}
		for _, b := range list {
			if m := greenBranchPattern.FindStringSubmatch(b.Name); m != nil {
				green[m[1]] = true
			}
		}
		if len(list) == 0 {
			return green, nil
		}
	}
}
