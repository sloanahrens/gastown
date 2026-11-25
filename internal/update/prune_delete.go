package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// pruneDeleteTimeout bounds one package version delete.
const pruneDeleteTimeout = 30 * time.Second

var (
	// pruneDeleteImagePattern is one repository name, the rule the read path
	// checks before an image name reaches a path.
	pruneDeleteImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// pruneDeleteTagPattern is a tag the packages API accepts; a tag reaches a
	// URL path, so it is checked before it is joined.
	pruneDeleteTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// DeletePruneTag deletes the container package version that image at tag names
// from the Forgejo packages API of the town's registry, through hc and the
// registry bot's basic credential. plaintextHost is the one non-loopback host
// the credential may reach over http (imagevalues.CheckCredentialTransport);
// "" is https only. A 2xx answer is success and any other status is an error
// naming the image, the tag and the status, never the answer's body. The
// credential never reaches an error.
func DeletePruneTag(ctx context.Context, hc *http.Client, reg imagevalues.Registry, plaintextHost, user, token, image, tag string) error {
	switch {
	case hc == nil:
		return errors.New("no HTTP client is wired for the registry delete")
	case !pruneDeleteImagePattern.MatchString(image):
		return fmt.Errorf("image name %q is not one repository name", image)
	case !pruneDeleteTagPattern.MatchString(tag):
		return fmt.Errorf("tag %q is not a tag the packages API accepts", tag)
	}
	if err := reg.Validate(); err != nil {
		return err
	}
	// The URL is built from parts Validate and the patterns above have already
	// checked, so it is assembled rather than parsed: no part can make it fail.
	u := &url.URL{Scheme: reg.Scheme, Host: reg.Host, Path: "/api/v1/packages/" + reg.Owner + "/container/" + image + "/" + tag}
	if err := imagevalues.CheckCredentialTransport(u, plaintextHost); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, pruneDeleteTimeout)
	defer cancel()
	req := (&http.Request{Method: http.MethodDelete, URL: u, Header: http.Header{}}).WithContext(ctx)
	req.SetBasicAuth(user, token)
	client := *hc
	client.CheckRedirect = imagevalues.SameEndpointRedirect
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting %s:%s from the packages API: %w", image, tag, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the packages API answered HTTP %d for the delete of %s:%s", resp.StatusCode, image, tag)
	}
	return nil
}
