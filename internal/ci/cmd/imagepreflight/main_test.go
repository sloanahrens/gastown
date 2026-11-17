package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/preflight"
	"github.com/steveyegge/gastown/internal/pincheck"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

const (
	green     = "0123456789abcdef0123456789abcdef01234567"
	tag       = "0123456789ab"
	mango     = "fedcba9876543210fedcba9876543210fedcba98"
	mangoURL  = "http://forgejo:3000/example-owner/mango.git"
	forgejo   = "forgejo:3000"
	envFile   = "env"
	noSuchJQ  = "no-such-jq"
	envOutput = "SHA=" + green + "\nTAG=" + tag + "\n"
)

// installed answers the tool lookup with a hit for every tool.
func installed(string) (string, error) { return "/usr/local/bin/tool", nil }

// jobArgs is the command line the workflow passes for a passing job.
func jobArgs(envPath string) []string {
	return []string{
		"-sha", green,
		"-mango-url", mangoURL,
		"-mango-commit", mango,
		"-host", forgejo,
		"-env-out", envPath,
	}
}

// greenVersion answers the version read with the base image's jq.
func greenVersion() (string, error) { return preflight.JQVersion, nil }

// The blobs every fake manifest names, so a dead one is a digest a test chose
// rather than a real layer. indexChild is the digest fakeRegistry's index names
// and is not a pin's digest, so the fake can tell the index from its child.
var (
	liveConfig = digestFor(0x01)
	liveLayer  = digestFor(0x02)
	deadLayer  = digestFor(0x03)
	indexChild = digestFor(0x11)
)

// digestFor is a digest of the byte k repeated, so each fake blob has its own.
func digestFor(k byte) string {
	return "sha256:" + strings.Repeat(fmt.Sprintf("%02x", k), 32)
}

// fakeRegistry answers the reference with an image manifest, except that Index
// makes it answer an index at its top reference whose linux/arm64 child is
// indexChild. It serves every blob but Dead, which it reports unserved for the
// repositories naming In; a ManifestErr or BlobErr is a transport failure it
// reports instead of an answer. Checking a layer's durability needs no socket:
// pincheck reads the Registry interface, and this is one.
type fakeRegistry struct {
	// Dead is the digest the fake reports unserved, for the repositories
	// naming In.
	Dead string
	In   string
	// Index answers an index rather than an image manifest, the shape every
	// pin in templates.DigestPinnedImages has upstream.
	Index bool
	// ManifestErr and BlobErr are transport failures the fake reports, so the
	// command's refusal of a registry that does not answer is driven through
	// it.
	ManifestErr error
	BlobErr     error
}

func (f fakeRegistry) Manifest(_ context.Context, repo, ref string) ([]byte, string, error) {
	if f.ManifestErr != nil {
		return nil, "", f.ManifestErr
	}
	if f.Index && ref != indexChild {
		return indexManifest(indexChild, pinnedPlatform), ociIndexType, nil
	}
	dead := f.Dead != "" && strings.Contains(repo, f.In)
	if f.Index {
		// An index has no layers of its own: Dead is in the child it names.
		dead = dead && ref == indexChild
	}
	layers := []string{liveLayer}
	if dead {
		layers = append(layers, f.Dead)
	}
	return imageManifest(liveConfig, layers...), ociManifestType, nil
}

func (f fakeRegistry) BlobExists(_ context.Context, _ string, digest string) (bool, error) {
	if f.BlobErr != nil {
		return false, f.BlobErr
	}
	return digest != f.Dead, nil
}

// The media types of the bodies fakeRegistry answers.
const (
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType    = "application/vnd.oci.image.index.v1+json"
)

// imageManifest is the body a registry answers for an image manifest.
func imageManifest(config string, layers ...string) []byte {
	entries := make([]string, 0, len(layers))
	for _, layer := range layers {
		entries = append(entries, fmt.Sprintf(`{"digest":%q,"size":1}`, layer))
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"digest":%q,"size":1},"layers":[%s]}`,
		ociManifestType, config, strings.Join(entries, ",")))
}

// indexManifest is the body a registry answers for an index naming one child
// for the platform.
func indexManifest(child, platform string) []byte {
	osName, arch, _ := strings.Cut(platform, "/")
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"digest":%q,"size":1,"platform":{"os":%q,"architecture":%q}}]}`,
		ociIndexType, child, osName, arch))
}

// TestRunAcceptsTheGreenCommit holds the passing branch: the command exits
// zero and hands the later steps the commit and its tag.
func TestRunAcceptsTheGreenCommit(t *testing.T) {
	t.Parallel()
	envPath := filepath.Join(t.TempDir(), envFile)
	var stdout, stderr strings.Builder

	if code := run(jobArgs(envPath), &stdout, &stderr, installed, greenVersion, fakeRegistry{}); code != 0 {
		t.Fatalf("run exited %d, stderr %q", code, stderr.String())
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("reading the environment file: %v", err)
	}
	if string(got) != envOutput {
		t.Errorf("the environment file holds %q, want %q", got, envOutput)
	}
	if !strings.Contains(stdout.String(), tag) {
		t.Errorf("the command printed %q, want the tag %s", stdout.String(), tag)
	}
}

// TestRunRefusesWhatThePreflightRefuses holds the refusal branch: a check the
// preflight fails exits non-zero, and the environment file carries no commit.
func TestRunRefusesWhatThePreflightRefuses(t *testing.T) {
	t.Parallel()
	envPath := filepath.Join(t.TempDir(), envFile)
	var stdout, stderr strings.Builder

	code := run(jobArgs(envPath), &stdout, &stderr, installed, func() (string, error) {
		return "jq-1.6", nil
	}, fakeRegistry{})
	if code == 0 {
		t.Fatalf("run passed with another jq, stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "jq-1.6") {
		t.Errorf("the command wrote %q to stderr, want the version it found", stderr.String())
	}
	if got, err := os.ReadFile(envPath); err != nil {
		t.Fatalf("reading the environment file: %v", err)
	} else if len(got) != 0 {
		t.Errorf("the environment file holds %q for a refused job", got)
	}
}

// TestRunRefusesACommandLineWithoutTheEnvFile holds the flag rule: the path
// the later steps read is required, and the command says so.
func TestRunRefusesACommandLineWithoutTheEnvFile(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder

	code := run([]string{"-sha", green}, &stdout, &stderr, installed, greenVersion, fakeRegistry{})
	if code == 0 {
		t.Fatalf("run passed without -env-out, stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "-env-out") {
		t.Errorf("the command wrote %q to stderr, want it to name -env-out", stderr.String())
	}
}

// TestRunRefusesAnEnvFileItCannotOpen holds the fail-closed branch: a file the
// runner cannot open fails the step instead of reporting a preflight it did
// not record.
func TestRunRefusesAnEnvFileItCannotOpen(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	envPath := filepath.Join(t.TempDir(), "absent", envFile)

	if code := run(jobArgs(envPath), &stdout, &stderr, installed, greenVersion, fakeRegistry{}); code == 0 {
		t.Fatalf("run passed with an environment file it could not open, stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), envPath) {
		t.Errorf("the command wrote %q to stderr, want it to name %s", stderr.String(), envPath)
	}
}

// TestJQVersionRefusesAProgramThatIsNotThere holds the version read's failure
// branch: a jq that does not run is an error, not an empty version.
func TestJQVersionRefusesAProgramThatIsNotThere(t *testing.T) {
	t.Parallel()
	if got, err := jqVersion(filepath.Join(t.TempDir(), noSuchJQ)); err == nil {
		t.Fatalf("jqVersion answered %q for a program that is not there", got)
	}
}

// TestImagePinDurabilityRefusesADeadLayer is the command's pin rule: a pinned
// image whose layer the registry no longer serves exits non-zero naming the
// image and the digest, with no commit left for the later steps, and every
// layer served exits zero. The index rows read the pin's index through its
// child, the shape every pin in templates.DigestPinnedImages has upstream.
func TestImagePinDurabilityRefusesADeadLayer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		reg  pincheck.Registry
		want []string
		// refuse rows exit non-zero; the rows without refuse are the jobs
		// that may build.
		refuse bool
	}{
		{
			name:   "a layer of the Dolt server pin",
			reg:    fakeRegistry{Dead: deadLayer, In: "dolt-sql-server"},
			want:   []string{templates.DoltServerImage, deadLayer + " (linux/arm64 layer 2)"},
			refuse: true,
		},
		{
			name:   "a layer of an index pin",
			reg:    fakeRegistry{Dead: deadLayer, In: "oci/docker", Index: true},
			want:   []string{templates.DindImage, deadLayer + " (linux/arm64 layer 2)"},
			refuse: true,
		},
		{
			name:   "no registry",
			reg:    nil,
			want:   []string{templates.DindImage, "no registry to ask"},
			refuse: true,
		},
		{"every layer served", fakeRegistry{}, nil, false},
		{"every layer of an index served", fakeRegistry{Index: true}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			envPath := filepath.Join(t.TempDir(), envFile)
			var stdout, stderr strings.Builder

			code := run(jobArgs(envPath), &stdout, &stderr, installed, greenVersion, tc.reg)
			got, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatalf("reading the environment file: %v", err)
			}
			if !tc.refuse {
				if code != 0 {
					t.Fatalf("run exited %d with every layer served, stderr %q", code, stderr.String())
				}
				if string(got) != envOutput {
					t.Errorf("the environment file holds %q, want %q", got, envOutput)
				}
				return
			}
			if code == 0 {
				t.Fatalf("run exited 0 with a dead layer, stdout %q", stdout.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to name %q", stderr.String(), want)
				}
			}
			if len(got) != 0 {
				t.Errorf("the environment file holds %q for a refused job", got)
			}
		})
	}
}

// TestImagePinDurabilityRefusesARegistryThatDoesNotAnswer is the command's
// transport rule: a registry whose manifest or blob request fails exits
// non-zero naming the pinned image and the failure, with no commit left for
// the later steps, rather than a build that started unverified.
func TestImagePinDurabilityRefusesARegistryThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	unreachable := errors.New("connection refused")
	cases := []struct {
		name string
		reg  fakeRegistry
	}{
		{"a manifest request", fakeRegistry{ManifestErr: unreachable}},
		{"a blob request", fakeRegistry{BlobErr: unreachable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			envPath := filepath.Join(t.TempDir(), envFile)
			var stdout, stderr strings.Builder

			code := run(jobArgs(envPath), &stdout, &stderr, installed, greenVersion, tc.reg)
			if code == 0 {
				t.Fatalf("run exited 0 with a registry that does not answer, stdout %q", stdout.String())
			}
			for _, want := range []string{templates.DindImage, unreachable.Error()} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			if got, err := os.ReadFile(envPath); err != nil {
				t.Fatalf("reading the environment file: %v", err)
			} else if len(got) != 0 {
				t.Errorf("the environment file holds %q for a refused job", got)
			}
		})
	}
}

// TestImagePinDurabilityRegistryRef holds the reference the registry is asked
// about: the tag a pin keeps for its readers is dropped, and a pin that names
// no registry host is qualified with Docker Hub's, the registry docker
// resolves it at.
func TestImagePinDurabilityRegistryRef(t *testing.T) {
	t.Parallel()
	dindDigest := pinnedDigest(templates.DindImage)
	hubDigest := digestFor(0x55)
	cases := []struct {
		name    string
		pinned  string
		want    string
		wantErr bool
	}{
		{
			name:   "a Forgejo pin",
			pinned: templates.DindImage,
			want:   templates.DindImage,
		},
		{
			name:   "a tag the pin keeps for its readers",
			pinned: "data.forgejo.org/oci/docker:v1@" + dindDigest,
			want:   "data.forgejo.org/oci/docker@" + dindDigest,
		},
		{
			name:   "an unqualified Docker Hub pin",
			pinned: templates.DoltServerImage,
			want:   "registry-1.docker.io/dolthub/dolt-sql-server@" + pinnedDigest(templates.DoltServerImage),
		},
		{
			name:   "a Docker Hub library pin",
			pinned: "postgres:17.11@" + hubDigest,
			want:   "registry-1.docker.io/library/postgres@" + hubDigest,
		},
		{"a pin without a digest", "data.forgejo.org/oci/docker", "", true},
		{"a pin without a repository", "data.forgejo.org/@" + hubDigest, "", true},
		{"an empty pin", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := registryRef(tc.pinned)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("registryRef(%q) = %q, want a refusal", tc.pinned, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("registryRef(%q) = error %v", tc.pinned, err)
			}
			if got != tc.want {
				t.Errorf("registryRef(%q) = %q, want %q", tc.pinned, got, tc.want)
			}
		})
	}
}

// TestImagePinDurabilityCheckRefusesAListOfNothing drives the check's own
// refusals: a pin the registry cannot be asked about, and a list holding no
// pin, are refusals rather than a build that started unverified.
func TestImagePinDurabilityCheckRefusesAListOfNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		refs []string
		want string
	}{
		{"a pin without a digest", []string{"data.forgejo.org/oci/docker"}, "is not repo@digest"},
		{"a pin without a repository", []string{"data.forgejo.org/@" + deadLayer}, "names no repository"},
		{"no pins", nil, "no digest-pinned image was named to check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkPinnedImages(context.Background(), fakeRegistry{}, tc.refs)
			if err == nil {
				t.Fatalf("checkPinnedImages(%q) passed", tc.refs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkPinnedImages(%q) = %q, want it to contain %q", tc.refs, err, tc.want)
			}
		})
	}
}

// pinnedDigest is the digest part of a pinned reference, after its '@'.
func pinnedDigest(pinned string) string {
	i := strings.LastIndexByte(pinned, '@')
	if i < 0 {
		return ""
	}
	return pinned[i+1:]
}
