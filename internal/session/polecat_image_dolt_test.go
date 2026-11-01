package session

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// TestPolecatImageInstallsDoltPinned pins how the image installs Dolt: the
// town's version in an ARG, one digest ARG per architecture the Dockerfile
// installs for, and a download that checks the digest before the binary reaches
// /usr/local/bin. The tier-sweep runner's doltserver, deps and cmd tests start
// a server or check its version on PATH, and an unchecked download would let a
// rebuild ship a different server than the town runs (gt-q7wkn).
// Mutation: drop the sha256sum check, a digest ARG, or the DOLT_VERSION ARG, or
// move DOLT_VERSION off templates.DoltVersion, and this goes red.
func TestPolecatImageInstallsDoltPinned(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)

	version, digests := "", map[string]string{}
	for _, in := range ins {
		rest, ok := strings.CutPrefix(in, "ARG ")
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		switch {
		case key == "DOLT_VERSION":
			version = value
		case strings.HasPrefix(key, "DOLT_SHA256_"):
			digests[strings.TrimPrefix(key, "DOLT_SHA256_")] = value
		}
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Errorf("the Dockerfile's DOLT_VERSION is %q, want a pinned x.y.z version so a rebuild keeps the town's server version", version)
	}
	if version != templates.DoltVersion {
		t.Errorf("the Dockerfile's DOLT_VERSION is %q, want %q (templates.DoltVersion): the image's client and the town's server are one version", version, templates.DoltVersion)
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, arch := range []string{"ARM64", "AMD64"} {
		digest, ok := digests[arch]
		if !ok {
			t.Errorf("the Dockerfile pins no DOLT_SHA256_%s, so an image for %s would install an unverified binary", arch, strings.ToLower(arch))
			continue
		}
		if !sha.MatchString(digest) {
			t.Errorf("DOLT_SHA256_%s is %q, want 64 lowercase hex digits", arch, digest)
		}
	}
	for arch := range digests {
		if arch != "ARM64" && arch != "AMD64" {
			t.Errorf("the Dockerfile pins a dolt digest for %q; the release ships arm64 and amd64 only", arch)
		}
	}

	var install string
	for _, in := range ins {
		if strings.HasPrefix(in, "RUN ") && strings.Contains(in, "DOLT_VERSION") {
			install = in
		}
	}
	if install == "" {
		t.Fatal("no RUN installs dolt from the version ARG")
	}
	for _, want := range []string{
		`https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-${ARCH}.tar.gz`,
		"dpkg --print-architecture",
		"DOLT_SHA256_ARM64",
		"DOLT_SHA256_AMD64",
		"sha256sum -c -",
		"/usr/local/bin/dolt",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("the dolt RUN lacks %q:\n%s", want, install)
		}
	}
	checked, placed := strings.Index(install, "sha256sum -c -"), strings.Index(install, "/usr/local/bin/dolt")
	if checked >= 0 && placed >= 0 && checked > placed {
		t.Errorf("the dolt RUN installs the binary before its digest check, so an unverified download reaches PATH:\n%s", install)
	}
	// An architecture with no digest in the case must stop the build, not fall
	// through to a download nothing verifies.
	fallback := strings.Index(install, "*)")
	if fallback < 0 || !strings.Contains(install[fallback:], "exit 1") {
		t.Errorf("the dolt RUN's architecture case has no arm that exits non-zero, so an unknown arch builds an unchecked binary:\n%s", install)
	}
}
