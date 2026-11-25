package cmd

import "fmt"

// refuseDowngrade returns nil when target may replace the running build: the same
// commit, a descendant of running, or allowRollback. Any other target is refused,
// naming both commits, and so is any unknown or unreadable input.
func refuseDowngrade(running, target string, allowRollback bool, isAncestor func(a, b string) (bool, error)) error {
	if running == "" {
		return fmt.Errorf("refusing to replace the build: the running commit is unknown")
	}
	if target == "" {
		return fmt.Errorf("refusing to replace %s: the target commit is empty", running)
	}
	if target == running || allowRollback {
		return nil
	}
	if isAncestor == nil {
		return fmt.Errorf("refusing downgrade of %s to %s: no ancestry reader is wired", running, target)
	}
	ancestor, err := isAncestor(running, target)
	if err != nil {
		return fmt.Errorf("refusing downgrade of %s to %s: reading ancestry: %w", running, target, err)
	}
	if ancestor {
		return nil
	}
	return fmt.Errorf("refusing downgrade of %s to %s: target does not descend from the running commit", running, target)
}
