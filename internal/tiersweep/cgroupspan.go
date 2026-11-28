package tiersweep

// measureCgroup samples the container's cgroup counters now and returns end,
// which samples them again and reports the growth since the first sample: what
// the container spent while the package ran. A nil sample gives an end that
// reports no sample and no error. A first sample that fails gives an end that
// reports that error and takes no second sample; a second sample that fails
// reports its own error.
func measureCgroup(sample func() (CgroupSample, error)) (end func() (*CgroupSample, error)) {
	if sample == nil {
		return func() (*CgroupSample, error) { return nil, nil }
	}
	first, err := sample()
	if err != nil {
		return func() (*CgroupSample, error) { return nil, err }
	}
	return func() (*CgroupSample, error) {
		second, err := sample()
		if err != nil {
			return nil, err
		}
		spent := second.Since(first)
		return &spent, nil
	}
}
