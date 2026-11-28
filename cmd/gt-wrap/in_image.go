package main

// inImageFor chooses the in-image check for a tool: the default target calls
// defaultCheck, and a configured target asks that target instead. An unreadable
// config returns an error with neither check run, so an unknown target is never
// checked as the default.
func inImageFor(s surface, name string, defaultCheck func(string) error) error {
	isDefault, err := usesDefaultTarget(s, name)
	if err != nil {
		return err
	}
	if isDefault {
		return defaultCheck(name)
	}
	return targetAnswers(s, name)
}
