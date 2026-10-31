package tiersweep

import (
	"fmt"
	"io"
	"path/filepath"
)

// gtBinName is the file name the sweep's gt binary is built under, in the log
// directory.
const gtBinName = "gt"

// gtBuildLDFlags is the link flag the gt binary needs to run at all. It is the
// flag buildGTBinary in internal/cmd/test_helpers_integration_test.go passes,
// so the binary built here is the one each helper would have built (gt-2daks).
const gtBuildLDFlags = "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1"

// buildGT builds the gt binary once for the sweep, into the log directory, and
// returns its path. The build runs in the root module through the Exec seam and
// takes the environment the sweep was started in, which carries the icu4c cgo
// flags the Makefile exports. A failed build prints one stderr line naming the
// exit code, and returns "": the sweep carries on with TIER_SWEEP_GT_BIN unset.
// The build's output is dropped, since it can hold a secret (rule 11).
func (s *Sweep) buildGT() string {
	bin := filepath.Join(s.Opts.LogDir, gtBinName)
	argv := []string{"go", "build", "-ldflags", gtBuildLDFlags, "-o", bin, "./cmd/gt"}
	if code := s.Exec(".", s.Environ, argv, io.Discard); code != 0 {
		fmt.Fprintf(s.Stderr, "%s: build gt: go build ./cmd/gt exited %d; %s is unset for the tests\n",
			Name, code, EnvGTBin)
		return ""
	}
	return bin
}
