package main

// usesDefaultTarget says whether the configured target for a tool is the default
// Docker one. A config that cannot be read is an error, never the default: the
// caller must not read an unknown target as the one it expects.
func usesDefaultTarget(s surface, name string) (bool, error) {
	cfg, err := configTarget(s, name)
	if err != nil {
		return false, err
	}
	return cfg.Target == defaultTarget, nil
}
