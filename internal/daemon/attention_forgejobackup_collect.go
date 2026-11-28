package daemon

import (
	"context"
	"errors"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

const hostProbeForgejoBackup = "forgejo backup"

// errForgejoBackupNotPublished says the Forgejo backup state exists only in the
// probe file, which the daemon reads in container mode. Outside container mode
// the read fails with this, so the dump duty raises its probe item rather than
// reading as a verified dump (gt-e8mwdq).
var errForgejoBackupNotPublished = errors.New("the Forgejo backup state is published only in the probe file, which the daemon reads in container mode")

// forgejoBackupProbe is the optional host read for the Forgejo dump duty: only
// the Mac's probe file carries it, so it stays off hostProbes and its fakes (gt-e8mwdq).
type forgejoBackupProbe interface {
	// ForgejoBackup is the published reading, nil for a file without the field.
	ForgejoBackup() (*hostprobe.Field[hostprobe.ForgejoBackup], error)
}

// ForgejoBackup is the probe file's field; a refused file is an error, never a
// nil field that would read as no dump recorded (gt-e8mwdq).
func (p fileHostProbes) ForgejoBackup() (*hostprobe.Field[hostprobe.ForgejoBackup], error) {
	pr, err := p.probes()
	if err != nil {
		return nil, err
	}
	return pr.ForgejoBackup, nil
}

// ForgejoBackup reports the dump duty as unreadable here: the machine the daemon
// runs on has no published backup state, so a silent nil would read as a
// verified dump (gt-e8mwdq).
func (systemHostProbes) ForgejoBackup() (*hostprobe.Field[hostprobe.ForgejoBackup], error) {
	return nil, errForgejoBackupNotPublished
}

// collectHostForgejoBackup raises the dump duty's item from the published
// reading; a refused file, or a host that cannot read it, keeps the item an
// earlier tick raised and raises the probe item (gt-e8mwdq).
func (s *attentionSources) collectHostForgejoBackup(context.Context) ([]attention.Item, error) {
	// Only a test fake lacks the method: every production host is a
	// fileHostProbes or a systemHostProbes, and both implement it (gt-e8mwdq).
	probe, ok := s.host.(forgejoBackupProbe)
	if !ok {
		return nil, nil
	}
	field, err := probe.ForgejoBackup()
	if err != nil {
		return s.hostProbeFailed(hostProbeForgejoBackup, err, attention.KindHostForgejoBackup), nil
	}
	if field == nil {
		return nil, nil
	}
	return attention.ForgejoBackupItems(field.Err, field.Value.LastGood, s.now), nil
}
