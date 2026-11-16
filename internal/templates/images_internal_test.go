package templates

import (
	"regexp"
	"testing"
)

// Image pin durability (gt-55fq3n): DindImage and SweepDindImage are both built
// from dockerImageDigest, the digest of the dind tag's image index, so moving
// the pin is one edit and the two references cannot drift apart.

var dockerImageDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TestImagePinDurabilityDindImagesShareTheDigest is a table over the two
// references built from dockerImageDigest: each is the docker repository plus
// that one constant, so both end in the digest it names.
//
// Mutation: give either reference a digest of its own, or move one and not the
// other, and a row goes red.
func TestImagePinDurabilityDindImagesShareTheDigest(t *testing.T) {
	t.Parallel()

	const repository = "data.forgejo.org/oci/docker@"
	if !dockerImageDigestRE.MatchString(dockerImageDigest) {
		t.Errorf("dockerImageDigest = %q, want sha256: and 64 lower-case hex digits", dockerImageDigest)
	}

	tests := []struct {
		name string
		ref  string
	}{
		{"DindImage", DindImage},
		{"SweepDindImage", SweepDindImage},
	}
	for _, tt := range tests {
		if want := repository + dockerImageDigest; tt.ref != want {
			t.Errorf("%s = %q, want %q", tt.name, tt.ref, want)
		}
	}

	if DindImage != SweepDindImage {
		t.Errorf("DindImage = %q and SweepDindImage = %q name different engines; want both built from dockerImageDigest", DindImage, SweepDindImage)
	}
}
