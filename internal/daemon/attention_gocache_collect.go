package daemon

import (
	"context"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/hostprobe"
)

const hostProbeGoCache = "go cache trim"

// goCacheTrimProbe is the optional host read for the Go cache duty. Only the
// Mac's probe file carries it, so the read stays off hostProbes and its fakes (gt-vlzm42).
type goCacheTrimProbe interface {
	// GoCacheTrim is the published reading, nil for a file without the field.
	GoCacheTrim() (*hostprobe.Field[hostprobe.GoCacheTrim], error)
}

// GoCacheTrim is the probe file's field; a refused file is an error, never a
// nil field that would read as no trim (gt-vlzm42).
func (p fileHostProbes) GoCacheTrim() (*hostprobe.Field[hostprobe.GoCacheTrim], error) {
	pr, err := p.probes()
	if err != nil {
		return nil, err
	}
	return pr.GoCacheTrim, nil
}

// collectHostGoCacheTrim raises the Go cache duty's item from the published
// reading; a refused file keeps the item an earlier tick raised (gt-vlzm42).
func (s *attentionSources) collectHostGoCacheTrim(context.Context) ([]attention.Item, error) {
	probe, ok := s.host.(goCacheTrimProbe)
	if !ok {
		return nil, nil
	}
	field, err := probe.GoCacheTrim()
	if err != nil {
		return s.hostProbeFailed(hostProbeGoCache, err, attention.KindHostGoCacheTrim), nil
	}
	if field == nil {
		return nil, nil
	}
	return attention.GoCacheTrimItems(field.Err, field.Value.LastGood, s.now), nil
}
