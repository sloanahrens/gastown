// Package preflight is the images workflow's first step (gt-gg9hv.62): the
// runner carries the tools the job runs, jq is the version the base image
// ships, the mango pin names a source on the Forgejo host, and the green
// commit is a full sha. The workflow text in deploy/ci/images.workflow.yml
// only calls it, so every refusal here is driven by a unit test through the
// injected seams.
package preflight

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Tools is every program the job runs: jq reads and writes the job's JSON, go
// builds and runs the workflow's own commands, docker builds and pushes, and
// git fetches the pinned sources.
var Tools = []string{"jq", "go", "docker", "git"}

// Schemes is every URL scheme the mango pin may name. The pin's host must also
// be the Forgejo host, so the read-only token never leaves the in-cluster
// host: the pins in deploy/daemon/image-pins are http to that host, and the
// credential helper is configured for these same schemes.
var Schemes = []string{"http", "https"}

// JQVersion is the jq the base image ships (deploy/ci-base/Dockerfile). The
// lesson of 2026-11-06 is that the runner's jq may be another version, so the
// preflight reads the version rather than trusting the label.
const JQVersion = "jq-1.7.1"

// fullSHA is the form of a commit the job may build at.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Options is the job's environment and the seams the checks run through.
type Options struct {
	// LookPath finds a tool; a tool it cannot find is missing.
	LookPath func(string) (string, error)
	// Version runs "jq --version" and answers what it printed.
	Version func() (string, error)
	// SHA is GITHUB_SHA, the green commit. MangoURL and MangoCommit are the
	// repository variables naming the mango source the daemon image carries.
	// Host is the Forgejo host:port the pins and the images name.
	SHA, MangoURL, MangoCommit, Host string
	// EnvOut takes the SHA= and TAG= lines the later steps read; a nil writer
	// is refused, so a checked commit cannot be dropped.
	EnvOut io.Writer
}

// Green is the commit the job builds at and the tag both images carry.
type Green struct {
	SHA string
	Tag string
}

// Check runs every preflight rule. It writes nothing to EnvOut unless all of
// them pass, so a step that fails leaves the later steps no commit to build.
func Check(o Options) (Green, error) {
	if o.LookPath == nil {
		return Green{}, errors.New("preflight: no tool lookup is wired")
	}
	if o.Version == nil {
		return Green{}, errors.New("preflight: no jq version read is wired")
	}
	if o.EnvOut == nil {
		return Green{}, errors.New("preflight: no environment output is wired")
	}
	var missing []string
	for _, tool := range Tools {
		if _, err := o.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return Green{}, fmt.Errorf("preflight: the ci-base runner is missing: %s", strings.Join(missing, " "))
	}
	// jq --version prints its version and nothing else, so naming it in the
	// error is the diagnostic and carries no other output.
	printed, err := o.Version()
	if err != nil {
		return Green{}, fmt.Errorf("preflight: jq --version did not answer: %w", err)
	}
	if got := strings.TrimSpace(printed); got != JQVersion {
		return Green{}, fmt.Errorf("preflight: jq --version printed %q, but this workflow needs %s (deploy/ci-base/Dockerfile); the base image's jq is another version", got, JQVersion)
	}
	if !fullSHA.MatchString(o.SHA) {
		return Green{}, fmt.Errorf("preflight: the green commit %q is not a full lowercase 40-character sha", o.SHA)
	}
	if err := o.checkMango(); err != nil {
		return Green{}, err
	}
	green := Green{SHA: o.SHA, Tag: o.SHA[:12]}
	if _, err := fmt.Fprintf(o.EnvOut, "SHA=%s\nTAG=%s\n", green.SHA, green.Tag); err != nil {
		return Green{}, fmt.Errorf("preflight: writing SHA and TAG to the environment: %w", err)
	}
	return green, nil
}

// checkMango holds the mango pin: an http(s) URL on the Forgejo host, the one
// host the read-only token is given to, at a full lowercase sha.
func (o Options) checkMango() error {
	if o.MangoURL == "" || o.MangoCommit == "" {
		return errors.New("preflight: the MANGO_URL and MANGO_COMMIT repository variables must name the mango source the daemon image carries")
	}
	u, err := url.Parse(o.MangoURL)
	if err != nil {
		return fmt.Errorf("preflight: MANGO_URL is not a URL: %w", err)
	}
	if !slices.Contains(Schemes, u.Scheme) {
		return fmt.Errorf("preflight: MANGO_URL %q is not an http or https URL", o.MangoURL)
	}
	if u.Host != o.Host {
		return fmt.Errorf("preflight: MANGO_URL names %q, but the read-only token is only given to the Forgejo host %q", u.Host, o.Host)
	}
	if !fullSHA.MatchString(o.MangoCommit) {
		return fmt.Errorf("preflight: MANGO_COMMIT %q is not a full lowercase 40-character sha", o.MangoCommit)
	}
	return nil
}
