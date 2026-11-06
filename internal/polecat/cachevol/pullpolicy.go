package cachevol

import "github.com/steveyegge/gastown/internal/imagepolicy"

// pullPolicyAlways is the pull policy for a container whose image reference is a moving tag.
const pullPolicyAlways = imagepolicy.PullPolicyAlways

// PullPolicyFor returns the pull policy for a container running image, by imagepolicy's one rule:
// the cache Jobs and the sweep Job must not decide it differently.
func PullPolicyFor(image string) string {
	return imagepolicy.PullPolicyFor(image)
}
