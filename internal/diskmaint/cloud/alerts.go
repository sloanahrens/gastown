package cloud

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

// containerIDPattern matches the short or full container ids docker ps prints.
// An id is checked against it before it reaches docker's command line.
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// dfArgs is the read: df's portable output for the filesystem a container's root
// sits on. That filesystem is the VM's own disk, where the Mac volume holding
// the VM's disk image is a different one.
var dfArgs = []string{"df", "-P", "/"}

var (
	// errListContainers reports a daemon that would not list its containers. The
	// runner's error carries the command's stderr, so it is dropped in favor of
	// this fixed reason.
	errListContainers = errors.New("list the cloud daemon's containers failed; the VM's disk cannot be read")
	// errBadContainerID reports a listed id that is not an id. It never reaches
	// the command line.
	errBadContainerID = errors.New("the daemon printed a container id that is not an id; the VM's disk cannot be read")
	// errNoContainers reports a daemon with no running container: nothing on it
	// can be asked for the VM's filesystem, and a disk the daemon cannot show is
	// not a quiet disk.
	errNoContainers = errors.New("the cloud daemon runs no container; the VM's disk cannot be read")
	// errDiskRead reports a df that did not run. The runner's error carries the
	// command's stderr, so it is dropped in favor of this fixed reason.
	errDiskRead = errors.New("read the VM's disk in a container failed; the disk cannot be read")
	// errParseDF reports df output with no usable capacity share. The parser's
	// message quotes the field it could not read, which is container output, so
	// the fixed reason replaces it.
	errParseDF = errors.New("df output names no usable capacity share; the VM's disk cannot be read")
)

// DiskAlerts returns the alert changes the cloud VM's disk carries now, read
// against the alert keys already open: diskmaint:cloud-vm at 80% used, cleared
// below 70%, and diskmaint:cloud-vm:unreadable for a disk the daemon cannot show.
func (t *Target) DiskAlerts(ctx context.Context, active map[string]bool) []disks.Alert {
	return disks.Evaluate([]disks.Reading{t.Reading(ctx)}, active)
}

// Reading reads the cloud VM's disk usage share through the VM's own docker
// daemon, or the error that stopped the read. It is alert-only: the VM's disk
// raises and clears its alerts but never tightens the town's registry keep.
func (t *Target) Reading(ctx context.Context) disks.Reading {
	r := disks.Reading{Name: readingName, AlertOnly: true}
	pct, err := t.diskShare(ctx)
	if err != nil {
		r.Err = err
		return r
	}
	r.UsedPct = pct
	return r
}

// diskShare reads the VM's used share as the worst one any container the daemon
// runs reports: a container's root filesystem is the VM's disk, and df there is
// the reading the Mac volume cannot give. One unreadable container refuses the
// whole reading, as the patrol's cluster read does, so a partial view cannot
// miss the full disk.
func (t *Target) diskShare(ctx context.Context) (float64, error) {
	out, err := t.client().Docker(ctx, "ps", "-q")
	if err != nil {
		return 0, errListContainers
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return 0, errNoContainers
	}
	var (
		worst    float64
		failures []error
	)
	for i, id := range ids {
		if !containerIDPattern.MatchString(id) {
			// The id is daemon text no validator has passed, so the refusal
			// names its position and not the text.
			failures = append(failures, fmt.Errorf("container %d: %w", i+1, errBadContainerID))
			continue
		}
		out, err := t.client().Docker(ctx, append([]string{"exec", id}, dfArgs...)...)
		if err != nil {
			failures = append(failures, fmt.Errorf("container %s: %w", id, errDiskRead))
			continue
		}
		pct, err := disks.ParseDF(string(out))
		if err != nil {
			failures = append(failures, fmt.Errorf("container %s: %w", id, errParseDF))
			continue
		}
		if pct > worst {
			worst = pct
		}
	}
	if len(failures) > 0 {
		return 0, errors.Join(failures...)
	}
	return worst, nil
}
