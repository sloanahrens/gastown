//go:build integration

package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestIntegrationCrossPlatformBuild verifies that the codebase compiles for every
// platform the tree still carries an implementation for. It catches a helper behind
// a build tag (//go:build darwin, //go:build !windows) that untagged code calls with
// no counterpart for the other platforms, which a host build never sees.
//
// A platform belongs in this list only while such counterparts exist. Windows is
// absent because its port was deleted (gt-638go.6) and it has none.
//
// It runs the go tool, so it lives in the integration tier.
func TestIntegrationCrossPlatformBuild(t *testing.T) {
	platforms := []struct {
		goos   string
		goarch string
		cgo    string
	}{
		{"linux", "amd64", "0"},
		{"linux", "arm64", "0"},
		{"darwin", "amd64", "0"},
		{"darwin", "arm64", "0"},
		{"freebsd", "amd64", "0"},
	}

	for _, p := range platforms {
		p := p // capture range variable
		t.Run(p.goos+"_"+p.goarch, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command("go", "build", "-o", os.DevNull, ".")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(),
				"GOOS="+p.goos,
				"GOARCH="+p.goarch,
				"CGO_ENABLED="+p.cgo,
			)

			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("build failed for %s/%s:\n%s", p.goos, p.goarch, string(output))
			}
		})
	}
}
