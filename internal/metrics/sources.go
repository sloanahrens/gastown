package metrics

// Sources is the registry of the snapshot's sources, one per group. Each
// source lives in its own source_<group>.go, so the bead that fills a group
// replaces that file and leaves this list alone.
func Sources() []Source {
	return []Source{
		queueSource{},
		newHostSource(),
		hostCPUSource{},
		moneySource{},
		reviewSource{},
		attentionSource{},
		configSource{},
		resourcesSource{},
		placementSource{},
		sweepSource{},
	}
}
