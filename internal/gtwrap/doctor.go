package gtwrap

import "fmt"

// Rung is a step of the ladder that runs a call, or the refusal when no step
// holds.
type Rung int

const (
	// RungExec runs the call in the running daemon container.
	RungExec Rung = iota
	// RungRun runs the call in a throwaway container of the daemon image.
	RungRun
	// RungDown is no step: Docker or the daemon cannot be reached, or their
	// state cannot be read.
	RungDown
)

// String is the rung's name, the word the doctor prints.
func (r Rung) String() string {
	switch r {
	case RungExec:
		return "exec"
	case RungRun:
		return "run"
	case RungDown:
		return "down"
	}
	return fmt.Sprintf("Rung(%d)", int(r))
}

// Diagnosis is what the doctor reports without running the tool: the rung a
// call would take, the container and image it would reach, and why.
type Diagnosis struct {
	Rung      Rung
	Container string
	Image     string
	Reason    string
}

// Doctor reports the rung Launch would use for a call, with the reason and the
// container and image it would reach, and runs nothing in the container. An
// unreadable compose file is an error: the container and image cannot be named,
// so there is no diagnosis to report.
func (d Docker) Doctor() (Diagnosis, error) {
	svc, err := d.daemonService()
	if err != nil {
		return Diagnosis{}, err
	}
	rung, reason := d.rung(svc)
	return Diagnosis{Rung: rung, Container: svc.ContainerName, Image: svc.Image, Reason: reason}, nil
}
