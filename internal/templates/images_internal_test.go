package templates

import (
	"regexp"
	"strings"
	"testing"
)

var dockerImageDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TestImagePinDurabilityDindImagesShareTheDigest is a table over the two
// references built from dockerImageDigest: each ends in the digest that one
// constant names, and both name the same engine, so moving the pin is one edit.
//
// Mutation: give either reference a digest of its own, or move one and not the
// other, and a row goes red.
func TestImagePinDurabilityDindImagesShareTheDigest(t *testing.T) {
	t.Parallel()

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
		if !strings.HasSuffix(tt.ref, "@"+dockerImageDigest) {
			t.Errorf("%s = %q, want it to end in the digest dockerImageDigest names, %q", tt.name, tt.ref, dockerImageDigest)
		}
	}

	if DindImage != SweepDindImage {
		t.Errorf("DindImage = %q and SweepDindImage = %q name different engines; want both built from dockerImageDigest", DindImage, SweepDindImage)
	}
}
