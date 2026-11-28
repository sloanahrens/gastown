package townflight

// Pending returns the labels of the listed commands still running. A listing or
// probe error is returned, never an empty list, so a drain cannot read a failed
// read as no command in flight.
func Pending(townRoot string) ([]string, error) {
	paths, err := listEntries(townRoot)
	if err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(paths))
	for _, path := range paths {
		label, running, err := probe(path)
		if err != nil {
			return nil, err
		}
		if running {
			labels = append(labels, label)
		}
	}
	return labels, nil
}
