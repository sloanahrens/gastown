package session

import (
	"slices"
	"strings"
	"testing"
)

// polecatImageBinaries are the town binaries the image copies to /usr/local/bin,
// each from bin/<name> in the build context.
var polecatImageBinaries = []string{"gt", "bd", "polecat-image", "sweep-run"}

// TestPolecatImageSetsBinaryModesWithCopyChmod pins that the town binaries get
// their mode from COPY --chmod=0755. A RUN chmod on the same files rewrites each
// one into a second layer, so the image carries every binary twice.
//
// Mutation: drop --chmod=0755 from any of the four COPY lines, or put back a
// RUN chmod that names one of their paths, and this goes red.
func TestPolecatImageSetsBinaryModesWithCopyChmod(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)

	for _, bin := range polecatImageBinaries {
		want := "COPY --chmod=0755 bin/" + bin + " /usr/local/bin/" + bin
		if !slices.Contains(ins, want) {
			t.Errorf("the Dockerfile has no %q: the mode must come from COPY --chmod=0755, not a RUN chmod that rewrites the file into a second layer", want)
		}
	}

	for _, in := range ins {
		if !strings.HasPrefix(in, "RUN ") || !strings.Contains(in, "chmod") {
			continue
		}
		fields := strings.Fields(in)
		for _, bin := range polecatImageBinaries {
			if slices.Contains(fields, "/usr/local/bin/"+bin) {
				t.Errorf("%q chmods /usr/local/bin/%s in a RUN, which writes a second copy of the binary into the image: set the mode with COPY --chmod=0755", in, bin)
			}
		}
	}
}
