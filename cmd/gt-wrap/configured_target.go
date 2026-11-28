package main

// configuredTarget opens the target the config names, for the tool the wrapper
// was invoked as: the config is read as a call of that tool reads it, the town is
// found, and the target is built under the tool's name. Each failure is returned
// with its reason, never a default target.
func configuredTarget(s surface, name string) (target, error) {
	cfg, err := configTarget(s, name)
	if err != nil {
		return nil, err
	}
	town, err := townFor(cfg, s)
	if err != nil {
		return nil, err
	}
	return openTarget(cfg, town, name, s)
}
