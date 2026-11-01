package session

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sweeprunner"
	"github.com/steveyegge/gastown/internal/templates"
)

// TestPolecatImageInstallsDockerCLIPinned pins the half of the tier-sweep
// runner that internal/testutil needs: a Docker client on PATH, so its
// `docker info` probe answers. The client is copied from the engine image the
// sweep's Docker-in-Docker sidecar runs, pinned by that image's digest, and the
// copy is the client alone: a daemon in the image every seat runs would be a
// capability no seat has today (gt-9lmam).
//
// Mutation: drop the COPY, the digest ARG or the digest pin in FROM, or copy a
// second binary out of the stage, and this goes red.
func TestPolecatImageInstallsDockerCLIPinned(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)

	args := map[string]string{}
	for _, in := range ins {
		rest, ok := strings.CutPrefix(in, "ARG ")
		if !ok {
			continue
		}
		if key, value, ok := strings.Cut(rest, "="); ok {
			args[key] = value
		}
	}
	digest, ok := args["DOCKER_CLI_DIGEST"]
	if !ok {
		t.Fatal("the Dockerfile pins no DOCKER_CLI_DIGEST, so a rebuild could pull a different docker client")
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		t.Errorf("DOCKER_CLI_DIGEST is %q, want sha256: and 64 lowercase hex digits", digest)
	}

	// The client must come from the engine the sweep's sidecar runs: one image
	// carries both halves, so an engine bump cannot silently leave a stale
	// client behind.
	var stage string
	for _, in := range ins {
		if rest, ok := strings.CutPrefix(in, "FROM "); ok && strings.HasSuffix(rest, " AS docker-cli") {
			stage = strings.TrimSuffix(rest, " AS docker-cli")
		}
	}
	if stage == "" {
		t.Fatal("the Dockerfile has no `FROM ... AS docker-cli` stage, so nothing offers the client to copy")
	}
	if got := strings.ReplaceAll(stage, "${DOCKER_CLI_DIGEST}", digest); got != templates.SweepDindImage {
		t.Errorf("the docker client stage is FROM %q, want %q (templates.SweepDindImage): the client and the sweep sidecar's engine are one image", got, templates.SweepDindImage)
	}

	var copies []string
	for _, in := range ins {
		if rest, ok := strings.CutPrefix(in, "COPY --from=docker-cli "); ok {
			copies = append(copies, rest)
		}
	}
	if want := []string{"/usr/local/bin/docker /usr/local/bin/docker"}; !slices.Equal(copies, want) {
		t.Errorf("the copies from the docker-cli stage are %q, want exactly %q: the client binary at a path on PATH, and no dockerd from the engine image", copies, want)
	}
}

// TestPolecatImageInstallsLsofPinned pins the tool the sweep runner's doltserver
// integration test needs to name the process holding a port: the image has no
// lsof or ss otherwise, and the test reads 0 from findDoltServerOnPort without
// one (gt-i109j). The package rides the same apt layer as the other base tools,
// at the version the LSOF_VERSION ARG names, so a rebuild cannot pull another.
//
// Mutation: delete lsof from the install line, or drop or empty the ARG, and
// this goes red.
func TestPolecatImageInstallsLsofPinned(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)

	version := ""
	for _, in := range ins {
		if rest, ok := strings.CutPrefix(in, "ARG LSOF_VERSION="); ok {
			version = rest
		}
	}
	if version == "" {
		t.Fatal("the Dockerfile pins no LSOF_VERSION, so a rebuild could install any lsof")
	}

	installs := 0
	for _, in := range ins {
		// The base-tools layer is the apt line that installs build-essential and
		// git beside it: the layer the bead names, not any apt line that happens
		// to carry the pin.
		if strings.HasPrefix(in, "RUN apt-get update") && strings.Contains(in, `"lsof=${LSOF_VERSION}"`) &&
			strings.Contains(in, "build-essential") && strings.Contains(in, " git ") {
			installs++
		}
	}
	if installs != 1 {
		t.Errorf("%d base-tools apt install lines (build-essential, git) name \"lsof=${LSOF_VERSION}\", want exactly 1: the image needs lsof pinned in the base-tools layer", installs)
	}
}

// TestPolecatImageCopiesTheSweepRunner pins that the image carries the program
// the runner container runs, at the path the sweep runner's backends name: a
// rename on either side would start a container whose command does not exist.
//
// Mutation: change the COPY's target, or sweeprunner.RunnerCommand, and this
// goes red.
func TestPolecatImageCopiesTheSweepRunner(t *testing.T) {
	t.Parallel()
	want := "COPY bin/sweep-run " + sweeprunner.RunnerCommand
	if !slices.Contains(dockerfileInstructions(t), want) {
		t.Errorf("the Dockerfile has no %q: the runner container runs sweeprunner.RunnerCommand, so the image must hold the binary there", want)
	}
}
