package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// pairDigestPrefixLength is how many hex digits of a digest a refusal names.
const pairDigestPrefixLength = 12

// pairMatchesDerived refuses a values file whose image pair is not the pair
// derived for its commit. The daemon and polecat images are always compared;
// the bd image only when the file names one. It returns nil when each compared
// image is the derived one, and otherwise names the first differing image with
// the first 12 hex of both digests. Not every error is a digest mismatch: it
// also refuses a derived pair with no full commit, a derived image that is
// empty, a file or derived image that is not a digest reference, and the same
// digest under another repository, each with its own message.
func pairMatchesDerived(file, derived imagevalues.Values) error {
	if !imagevalues.ValidCommit(derived.Commit) {
		return fmt.Errorf("the derived pair has no full commit, so the file cannot be matched to a derivation")
	}
	if err := compareDerivedImage("daemon", file.Daemon, derived.Daemon); err != nil {
		return err
	}
	if err := compareDerivedImage("polecat", file.Polecat, derived.Polecat); err != nil {
		return err
	}
	if file.Bd == "" {
		return nil
	}
	return compareDerivedImage("bd", file.Bd, derived.Bd)
}

// compareDerivedImage compares one image of the file with the derived one. Both
// must be digest references, and the derived one must exist: an image the
// derivation lacks is never a match.
func compareDerivedImage(image, file, derived string) error {
	if derived == "" {
		return fmt.Errorf("the derived pair has no %s image, so the file's %s image cannot match it", image, image)
	}
	if !imagevalues.ValidRef(file) {
		return fmt.Errorf("the file's %s image %q is not a digest reference", image, file)
	}
	if !imagevalues.ValidRef(derived) {
		return fmt.Errorf("the derived %s image %q is not a digest reference", image, derived)
	}
	if file == derived {
		return nil
	}
	fileDigest := refDigest(file)
	derivedDigest := refDigest(derived)
	if fileDigest == derivedDigest {
		return fmt.Errorf("the file's %s image and the derived %s image share a digest but name different repositories", image, image)
	}
	return fmt.Errorf("the file's %s digest sha256:%s differs from the derived %s digest sha256:%s",
		image, digestPrefix(fileDigest), image, digestPrefix(derivedDigest))
}

// refDigest is the digest a validated reference names, the part after '@'.
func refDigest(ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return digest
}

// digestPrefix is the first 12 hex of a sha256 digest, the part a refusal
// names. The caller passes a digest from a validated reference.
func digestPrefix(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")[:pairDigestPrefixLength]
}
