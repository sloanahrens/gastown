package sweeprunner

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/templates"
)

// DefaultSeedImages are the images the packages' tests start in the sidecar's
// engine: the Dolt server the Dolt-backed tests run (internal/testutil's
// DoltDockerImage; a test keeps the two equal). They are pulled by the tag the
// tests name, since the tests start that tag; the sidecar engine itself is
// pinned by digest (SweepDindImage).
var DefaultSeedImages = []string{"dolthub/dolt-sql-server:2.0.7"}

const (
	// seedName is the temporary engine container a seed run starts.
	seedName = "gt-sweep-seed"
	// seedReadyTries bounds how long the seed waits for its engine: one try a second.
	seedReadyTries = 60
)

// SeedSpec is one seed run: the engine context and the images to pull.
type SeedSpec struct {
	// Context is the Docker CLI context of the engine the sweep runs on.
	Context string
	// Images are the references to pull into the seed volume.
	Images []string
}

// Validate refuses a seed run docker would read as something else.
func (s SeedSpec) Validate() error {
	if s.Context == "" || hasControl(s.Context) || s.Context[0] == '-' {
		return &FieldError{Field: "context", Rule: "is empty, holds a control character or reads as a flag"}
	}
	if len(s.Images) == 0 {
		return &FieldError{Field: "images", Rule: "names no image to pull"}
	}
	for _, image := range s.Images {
		if !imageReference(image) {
			return &FieldError{Field: "images", Rule: fmt.Sprintf("%q is not an image reference", image)}
		}
	}
	return nil
}

// RenderSeed returns the steps of the one-time seed: a temporary engine with
// network access on the seed volume pulls each image, then stops cleanly so the
// engine state is whole, and teardown removes the container. The seed volume
// stays; the next seed run adds to it. Run it while no sweep is running: the
// engine state is written by one engine at a time.
func (Docker) RenderSeed(seed SeedSpec) (Plan, error) {
	if err := seed.Validate(); err != nil {
		return Plan{}, err
	}
	spec := Spec{Context: seed.Context}
	steps := []Step{
		dockerStep(spec, "volume", "create", SeedVolume),
		dockerStep(spec, "run", "-d",
			"--name", seedName,
			"--privileged",
			"--env", "DOCKER_TLS_CERTDIR=",
			"--volume", SeedVolume+":"+dindDataPath,
			templates.SweepDindImage,
			"dockerd",
		),
		// The start step returns before dockerd listens; wait for it, bounded.
		dockerStep(spec, "exec", seedName, "sh", "-c",
			fmt.Sprintf("i=0; while ! docker info >/dev/null 2>&1; do i=$((i+1)); [ $i -gt %d ] && exit 1; sleep 1; done", seedReadyTries)),
	}
	for _, image := range seed.Images {
		steps = append(steps, dockerStep(spec, "exec", seedName, "docker", "pull", image))
	}
	steps = append(steps,
		dockerStep(spec, "stop", "-t", "30", seedName),
		teardownStep(spec, "rm", "-f", seedName),
	)
	return Plan{Steps: steps}, nil
}
