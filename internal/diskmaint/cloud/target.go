// Package cloud prunes the gt-cloud Colima VM's docker images as a diskmaint
// target. Every read goes through the VM's own daemon socket, which the caller
// names, so the measure is the daemon's view of what fills the VM's disk and
// never the Mac volume the VM's disk image lives on.
//
// The keep rules and the plan are in plan.go; the apply is in apply.go.
package cloud

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
)

const (
	// planTarget is the Target name a plan and a ledger entry carry.
	planTarget = "cloud"
	// readingName is the name the cloud VM's disk carries in a measurement's
	// Detail.
	readingName = "cloud-vm"
)

var (
	// errNoDockerHost reports a target built without the cloud daemon's socket.
	// Without it docker would talk to whatever daemon the environment picks.
	errNoDockerHost = errors.New("no docker host configured; the cloud daemon is not named")
	// errNoRunner reports a target built with no docker runner: nothing can be
	// read, so no target is built.
	errNoRunner = errors.New("no docker runner configured")
	// errNoLedger reports a target built without a ledger. Every apply must
	// leave a record, so a target that cannot write one is not built.
	errNoLedger = errors.New("cloud: no ledger configured")
)

// Config is what the cloud target needs to measure, plan and apply.
type Config struct {
	// DockerHost is the cloud VM's daemon socket, passed to every docker call as
	// DOCKER_HOST, e.g. unix://<home>/.colima/cloud/docker.sock.
	DockerHost string
	// Run runs docker on the daemon named by DockerHost.
	Run diskmaint.Runner
	// Ledger records every apply; a target without one is not built.
	Ledger *diskmaint.Ledger
	// LabelsFile is the file holding the cloud VM's runner labels, one per line
	// as forgejo-runner register was given them ("<name>:docker://<ref>").
	// Plan reads it through Run and keeps the images the labels name; empty names
	// no list, and Plan refuses rather than keeping a list of its own.
	LabelsFile string
	// DeployFile is the app's deploy workflow. Plan reads it through Run and
	// keeps every fractals-nextjs or deploy-base image the workflow names; empty
	// names no workflow, and Plan refuses rather than assuming the workflow
	// names none.
	DeployFile string
}

// Target is the cloud VM's docker daemon as a diskmaint target.
type Target struct {
	cfg Config
}

// NewTarget returns a Target that reads the daemon named by cfg, or the error
// for the config field it leaves out, so no caller holds a target that cannot
// read or cannot record.
func NewTarget(cfg Config) (*Target, error) {
	switch {
	case cfg.DockerHost == "":
		return nil, errNoDockerHost
	case cfg.Run == nil:
		return nil, errNoRunner
	case cfg.Ledger == nil:
		return nil, errNoLedger
	}
	return &Target{cfg: cfg}, nil
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// Measure reports the bytes the daemon's images hold as the daemon lists them;
// an unreadable image list or size is an error, never a zero.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	images, err := t.client().Images(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	// docker prints one row per repo:tag, each with the same id and size, so an
	// image with several tags is counted once.
	seen := make(map[string]bool, len(images))
	var used int64
	for _, im := range images {
		if seen[im.ID] {
			continue
		}
		seen[im.ID] = true
		// An image whose size does not parse refuses the whole reading: a
		// partial sum undercounts and reads as a fact.
		n, err := dockerimg.ParseSize(im.Size)
		if err != nil {
			return diskmaint.Measurement{}, fmt.Errorf("measure image %s: %w", im.ID, err)
		}
		used += n
	}
	return diskmaint.Measurement{
		Used:   used,
		Detail: fmt.Sprintf("%s: %d images on the daemon", readingName, len(seen)),
	}, nil
}

// client is the shared docker reader pointed at the cloud VM's daemon.
func (t *Target) client() dockerimg.Client {
	return dockerimg.Client{Host: t.cfg.DockerHost, Run: t.cfg.Run}
}
