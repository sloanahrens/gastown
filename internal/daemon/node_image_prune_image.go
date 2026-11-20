package daemon

import (
	"errors"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// nodePruneImage is the running values file's Polecat reference, the image the node prune Job runs.
func (d *Daemon) nodePruneImage() (string, error) {
	running, _, err := d.runningImages()
	if err != nil {
		return "", err
	}
	if !imagevalues.ValidRef(running.Polecat) {
		return "", errors.New("no running polecat image: the values file is missing or its polecat is not a digest reference")
	}
	return running.Polecat, nil
}
