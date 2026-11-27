package attention

import "fmt"

// OverrunBlindItem returns no item below three blind ticks in a row and one
// info item at three or more, naming the seats the last tick could not judge
// and the first read error (gt-13d389).
func OverrunBlindItem(streak, seats int, firstErr string) (Item, bool) {
	if streak < 3 {
		return Item{}, false
	}
	return Item{
		Key:      "overrun-guard:blind",
		Kind:     KindOverrunBlind,
		Severity: SeverityLow,
		Summary: fmt.Sprintf("overrun guard blind for %d ticks in a row: %d seats could not be judged; first read error: %s",
			streak, seats, firstErr),
	}, true
}
