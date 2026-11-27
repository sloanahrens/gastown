package tiersweep

// serialUnits cuts the run-alone tasks into the units that run one after
// another: a package's shards are one unit, and every other task is a unit of
// one. A unit stands where its first task stood; nil input returns nil.
func serialUnits(serial []*task) [][]*task {
	if serial == nil {
		return nil
	}
	units := make([][]*task, 0, len(serial))
	unit := make(map[string]int)
	for _, t := range serial {
		if t.shard == nil {
			units = append(units, []*task{t})
			continue
		}
		if i, ok := unit[t.name]; ok {
			units[i] = append(units[i], t)
			continue
		}
		unit[t.name] = len(units)
		units = append(units, []*task{t})
	}
	return units
}
