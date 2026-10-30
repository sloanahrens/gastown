// Package sweeprunner renders one tier-sweep run as the steps that start it off
// the host: a Spec of the run, and a Backend that turns it into argv (Docker) or
// a Job manifest (Kubernetes). Rendering only: nothing here runs a step, so the
// gate proves what a run would execute without Docker, a cluster or a sweep
// (gt-74ix9).
package sweeprunner

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// Spec is one sweep run request: what runs, where it reads and writes, and the
// limits it runs under.
type Spec struct {
	// Rig is the rig whose sweep runs. It is one path segment, because it is
	// part of every container, network and volume name the run makes.
	Rig string
	// SHA is the 40-hex commit the run tests.
	SHA string
	// Image is the container the run's steps use: the polecat's image tag, or
	// the same reference pinned by digest.
	Image string
	// CPUs is the runner's --cpus limit, 1 to 16.
	CPUs int
	// MemoryGB is the runner's memory limit in GB, 2 to 48.
	MemoryGB int
	// Tiers are the tiers to run, in the order given. Empty runs the tiers a
	// tier-sweep command line naming none runs (TiersOrDefault).
	Tiers []string
	// Jobs is how many packages the run tests at once, 1 to 16.
	Jobs int
	// OutDir is the host directory the runner writes result.json and its logs
	// into, mounted at OutputDir. It must be absolute.
	OutDir string
	// RepoDir is the rig's bare repository, mounted read-only at RepoMount. It
	// must be absolute.
	RepoDir string
	// Dind starts a Docker-in-Docker sidecar for the packages whose tests need a
	// Docker API (internal/testutil starts Dolt through testcontainers). The
	// runner never mounts the host's socket.
	Dind bool
	// RunID names this launch and must be unique to it. Every container, network
	// and volume the run makes carries it, so two runs of one rig and sha never
	// name the same object and one run's teardown cannot remove another's
	// (gt-74ix9).
	RunID string
	// Context is the context the steps run against on the backend's engine: the
	// Docker CLI context (desktop-linux, colima-gt-sbx) or the kubectl context.
	// It is a field so a render names its engine instead of taking whichever one
	// the environment happens to select.
	Context string
}

// FieldError is a Spec field whose value a render cannot use, named so the
// caller can report which input to fix.
type FieldError struct {
	// Field is the Spec field's name, without the spec. prefix Error prints.
	Field string
	// Rule is what the value must be.
	Rule string
}

// Error names the field and the rule it broke.
func (e *FieldError) Error() string {
	return "sweeprunner: spec." + e.Field + ": " + e.Rule
}

// shaPattern is a git commit as git prints it.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Validate refuses a Spec no backend can render into a run that is safe to
// start, naming the field that failed.
func (s Spec) Validate() error {
	if !nameSegment(s.Rig) {
		return &FieldError{Field: "rig", Rule: fmt.Sprintf("%q is not one path segment", s.Rig)}
	}
	if !shaPattern.MatchString(s.SHA) {
		return &FieldError{Field: "sha", Rule: fmt.Sprintf("%q is not 40 lower-case hex digits", s.SHA)}
	}
	if !imageReference(s.Image) {
		return &FieldError{Field: "image", Rule: fmt.Sprintf("%q is not an image reference", s.Image)}
	}
	if s.CPUs < 1 || s.CPUs > 16 {
		return &FieldError{Field: "cpus", Rule: fmt.Sprintf("%d is outside 1-16", s.CPUs)}
	}
	if s.MemoryGB < 2 || s.MemoryGB > 48 {
		return &FieldError{Field: "memoryGB", Rule: fmt.Sprintf("%d is outside 2-48", s.MemoryGB)}
	}
	if s.Jobs < 1 || s.Jobs > 16 {
		return &FieldError{Field: "jobs", Rule: fmt.Sprintf("%d is outside 1-16", s.Jobs)}
	}
	seen := make(map[string]bool, len(s.Tiers))
	for _, tier := range s.Tiers {
		if !tiersweep.KnownTier(tier) {
			return &FieldError{Field: "tiers", Rule: fmt.Sprintf("%q is not a tier the sweep runs", tier)}
		}
		if seen[tier] {
			return &FieldError{Field: "tiers", Rule: fmt.Sprintf("%q appears twice", tier)}
		}
		seen[tier] = true
	}
	if !mountPath(s.OutDir) {
		return &FieldError{Field: "outDir", Rule: fmt.Sprintf("%q is not an absolute path without a colon", s.OutDir)}
	}
	if !mountPath(s.RepoDir) {
		return &FieldError{Field: "repoDir", Rule: fmt.Sprintf("%q is not an absolute path without a colon", s.RepoDir)}
	}
	if !nameSegment(s.Context) {
		return &FieldError{Field: "context", Rule: fmt.Sprintf("%q is not one Docker context name", s.Context)}
	}
	if !nameSegment(s.RunID) {
		return &FieldError{Field: "runID", Rule: fmt.Sprintf("%q is not one name segment; every object the run makes is named after it", s.RunID)}
	}
	return nil
}

// defaultTiers are the tiers a run naming none tests: the same three, in the
// same order, that a tier-sweep command line naming none runs, so a sweep
// through the runner tests what a host sweep tests. A test resolves the sweep's
// own default and holds the two lists equal (gt-74ix9).
var defaultTiers = []string{tiersweep.TierShell, tiersweep.TierIntegration, tiersweep.TierRace}

// TiersOrDefault returns the tiers to run, in order: the Spec's own, or
// defaultTiers when the Spec names none.
func (s Spec) TiersOrDefault() []string {
	if len(s.Tiers) > 0 {
		return s.Tiers
	}
	return append([]string(nil), defaultTiers...)
}

// nameSegment reports whether v can stand as one Docker name: non-empty, one
// path segment, no colon or whitespace, and nothing that reads as a flag.
func nameSegment(v string) bool {
	if v == "" || v == "." || v == ".." || strings.HasPrefix(v, "-") {
		return false
	}
	for _, r := range v {
		if r == '/' || r == '\\' || r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// mountPath reports whether dir can stand beside a container path in a -v
// argument: absolute, and no colon the mount syntax would read as a separator.
func mountPath(dir string) bool {
	return dir != "" && !strings.Contains(dir, ":") && filepath.IsAbs(dir) && !hasControl(dir)
}

// imageReference reports whether image can be the last argument of a run: a
// reference, not an empty string and not something docker would read as a flag.
func imageReference(image string) bool {
	return image != "" && !strings.HasPrefix(image, "-") && !hasControl(image) &&
		!strings.ContainsFunc(image, unicode.IsSpace)
}

// hasControl reports whether v holds a control character.
func hasControl(v string) bool {
	return strings.ContainsFunc(v, unicode.IsControl)
}
