package land

import "strings"

// IsHumanActor reports whether actor is the human's recorded identity, the
// identity a submission must never carry (gt-2ge3z).
//
// The human is the town's overseer, whose name is configuration and is passed
// in; nothing here names a person. The comparison is the whole identity, never
// a substring: an agent whose name contains the human's is its own identity,
// not the human.
func IsHumanActor(actor, human string) bool {
	human = strings.TrimSpace(human)
	return human != "" && strings.TrimSpace(actor) == human
}
