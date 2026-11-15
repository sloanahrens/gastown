package taskmap

import (
	"strings"
	"testing"
)

// TestMetricsRunUseDoesNotRestateTheWeeklyJob pins the rule for the metrics run
// entry (gt-rknkdx): the command's own help states that the hour retrains and
// adopts the story-point weights weekly, so the task map's Use names the
// situation alone rather than carrying a second copy of that sentence.
func TestMetricsRunUseDoesNotRestateTheWeeklyJob(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("metrics run")
	if !ok {
		t.Fatal(`Lookup("metrics run") found no entry`)
	}
	for _, restated := range []string{"weekly", "story-point", "retrain", "adoption"} {
		if strings.Contains(e.Use, restated) {
			t.Errorf("metrics run Use = %q; it restates the weekly job (%q) that the command's help already states", e.Use, restated)
		}
	}
}
