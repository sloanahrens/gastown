package metrics

import "runtime"

// Sources is the registry of the snapshot's sources, one per group. Each
// source lives in its own source_<group>.go, so the bead that fills a group
// replaces that file and leaves this list alone.
func Sources() []Source {
	return []Source{
		queueSource{},
		newHostSource(),
		newHostCPUSource(runtime.GOOS),
		moneySource{},
		reviewSource{},
		attentionSource{},
		configSource{},
		resourcesSource{},
		placementSource{},
		sweepSource{},
	}
}

// newHostCPUSource builds the host CPU source for the host named by goos. The
// group reads the Mac's process table, which a daemon in a pod does not have,
// so off darwin it is blind and refuses rather than writing nulls. The OS name
// is a parameter so the gate on Linux tests both sides.
func newHostCPUSource(goos string) hostCPUSource {
	return hostCPUSource{blind: goos != "darwin"}
}
