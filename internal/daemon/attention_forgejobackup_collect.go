package daemon

import (
	"context"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

const hostProbeForgejoBackup = "forgejo backup"

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

// collectHostForgejoBackup raises the dump duty's item from the published
// reading; a refused file keeps the item an earlier tick raised (gt-e8mwdq).
func (s *attentionSources) collectHostForgejoBackup(context.Context) ([]attention.Item, error) {
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
