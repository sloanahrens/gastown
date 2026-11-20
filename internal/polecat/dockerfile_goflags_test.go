package polecat

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// TestPolecatImageGOFLAGS pins the image's GOFLAGS: -gcflags is part of Go's
// build cache key, so a seat compiled without the seed's flag reuses nothing (gt-75l2ij).
func TestPolecatImageGOFLAGS(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../deploy/polecat/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	// One instruction per line, so the ENV block's continuations join.
	assignment := regexp.MustCompile(`(?m)^ENV\b.*\bGOFLAGS=(?:"([^"]*)"|(\S+))`)
	found := assignment.FindAllStringSubmatch(strings.ReplaceAll(string(data), "\\\n", " "), -1)
	if len(found) == 0 {
		t.Fatal("deploy/polecat/Dockerfile has no ENV instruction setting GOFLAGS")
	}
	last := found[len(found)-1]
	if got, want := last[1]+last[2], "-p=4 -gcflags=all=-dwarf=false"; got != want {
		t.Errorf("ENV GOFLAGS = %q, want %q", got, want)
	}
}

// TestPolecatImageGOFLAGSMatchesSeedJob ties the image's ENV GOFLAGS to the seed job's constant (gt-8qq6qf).
func TestPolecatImageGOFLAGSMatchesSeedJob(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../deploy/polecat/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	assignment := regexp.MustCompile(`(?m)^ENV\b.*\bGOFLAGS=(?:"([^"]*)"|(\S+))`)
	found := assignment.FindAllStringSubmatch(strings.ReplaceAll(string(data), "\\\n", " "), -1)
	if len(found) == 0 {
		t.Fatal("deploy/polecat/Dockerfile has no ENV instruction setting GOFLAGS")
	}
	last := found[len(found)-1]
	if got, want := last[1]+last[2], "-p=4 "+cachevol.GoCompileFlags; got != want {
		t.Errorf("ENV GOFLAGS = %q, want %q: the image and the seed job must pass the same compile flags or the seed's entries go unused", got, want)
	}
}
