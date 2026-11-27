package attention

import (
	"fmt"
	"time"
)

// goCacheTrimStaleAfter is the age past which the last Go cache trim raises the
// item (gt-a0sewj).
const goCacheTrimStaleAfter = 12 * time.Hour

// GoCacheTrimItems returns one low host-gocache-trim item for a failed trim,
// carrying failure, for no completed trim, or for a last trim more than 12 hours
// before now (gt-a0sewj).
func GoCacheTrimItems(failure string, lastGood, now time.Time) []Item {
	if failure != "" {
		return []Item{goCacheTrimItem(fmt.Sprintf(
			"Go cache trim failed: %s; the next go command may pay a full trim", failure))}
	}
	if lastGood.IsZero() {
		return []Item{goCacheTrimItem(
			"no Go cache trim has completed; the next go command may pay a full trim")}
	}
	if age := now.Sub(lastGood); age > goCacheTrimStaleAfter {
		return []Item{goCacheTrimItem(fmt.Sprintf(
			"last Go cache trim completed %s ago; the next go command may pay a full trim",
			age.Truncate(time.Minute)))}
	}
	return nil
}

// goCacheTrimItem is the one item every Go cache trim condition raises, keyed by its rule (gt-a0sewj).
func goCacheTrimItem(summary string) Item {
	return Item{
		Key:      RuleHostGoCacheTrim,
		Kind:     KindHostGoCacheTrim,
		Severity: SeverityLow,
		Summary:  summary,
	}
}
