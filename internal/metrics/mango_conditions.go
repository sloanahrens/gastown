package metrics

// MangoNarrowBelow is the share of the prepared landings under which a box
// condition narrows the group: a condition true of less than this fraction is
// one that singles out a few landings, so the panel draws it as a chip instead
// of tucking it away with the conditions true of nearly everything.
const MangoNarrowBelow = 0.90

// MangoMaxConditions caps the conditions one box contributes to the panel, so
// a wide box cannot fill the row it is drawn in.
const MangoMaxConditions = 16

// MangoCondition is one range of a best box read alone, as the panel writes it:
// the range on its own, as Describe renders it, the share of the prepared rows
// that satisfy it by itself, and whether that share is below MangoNarrowBelow.
type MangoCondition struct {
	Text    string  `json:"text"`
	Share   float64 `json:"share"`
	Narrows bool    `json:"narrows"`
}

// MangoConditions splits cube into its single conditions, in the box's own
// order, capped at MangoMaxConditions. Each condition is one range of the box
// described alone, such as "priority in [1, 2]", and holds the fraction of
// rows that satisfy that range by itself; a condition holds of every row it
// shares with the box, so its share is at least the box's own.
//
// A cube with more than MangoMaxConditions ranges contributes its first
// MangoMaxConditions only, and one with no rows gives every condition a share
// of 0, as fraction does.
func MangoConditions(cube Cube, rows []Row) []MangoCondition {
	conditions := make([]MangoCondition, 0, len(cube.Ranges))
	for i, r := range cube.Ranges {
		if i == MangoMaxConditions {
			break
		}
		one := Cube{Ranges: []Range{r}}
		matched := 0
		for _, row := range rows {
			if Matches(one, row) {
				matched++
			}
		}
		share := fraction(matched, len(rows))
		conditions = append(conditions, MangoCondition{
			Text:    Describe(one),
			Share:   share,
			Narrows: share < MangoNarrowBelow,
		})
	}
	return conditions
}
