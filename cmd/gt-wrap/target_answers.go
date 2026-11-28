package main

// targetAnswers is nil when the tool's configured target answers: configuredTarget
// opens the target the config names, and doctorAnswers judges its doctor. Either
// failure is returned with its reason, so a caller never reads an unanswerable
// target as one it may install into.
func targetAnswers(s surface, name string) error {
	t, err := configuredTarget(s, name)
	if err != nil {
		return err
	}
	return doctorAnswers(t)
}
