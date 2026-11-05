package cachevol

import "strings"

// pullPolicyAlways is the pull policy for a container whose image reference is a moving tag.
const pullPolicyAlways = "Always"

// PullPolicyFor returns the pull policy for a container running image: pullPolicyAlways when the
// reference names a moving tag (dev, latest, or no tag, which Kubernetes reads as latest), and the
// empty policy otherwise.
//
// A digest names one image, and a tag of seven or more lowercase hex characters names the commit it
// was built from: a node's cached copy of either is the one the reference asks for. Any other tag is
// fixed enough that the cluster's default, IfNotPresent for a non-latest tag, is right for it, and
// a tag that merely contains a moving tag's name, such as devel, is not itself one.
func PullPolicyFor(image string) string {
	if strings.Contains(image, "@") {
		return ""
	}
	switch imageTag(image) {
	case "", "dev", "latest":
		return pullPolicyAlways
	}
	return ""
}

// imageTag is the tag in image, or "" when it names none. The tag follows the last colon of the last
// path segment, so a registry's port (localhost:5000/repo) is not read as one.
func imageTag(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return ""
	}
	return name[i+1:]
}
