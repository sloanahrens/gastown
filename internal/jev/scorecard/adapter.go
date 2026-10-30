package scorecard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// QuestionID names one question in a battery's log line.
type QuestionID string

// Answer is Jev's answer to one question. A noul sets Noul, a score sets
// Score, and a choice sets Choice and Probabilities with Positive naming the
// option that counts as a yes.
type Answer struct {
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Positive      string             `json:"positive,omitempty"`
}

// Kind names the answer's kind, KindNoul, KindScore or KindChoice, or the empty
// string when the answer carries none of them.
func (ans Answer) Kind() string { return kindOf(ans) }

// Value reports the number a question of kind scores ans by — a noul's
// probability, a score's level, or a choice's probability for its positive
// option — and whether chosen marks the positive option. ok is false when the
// answer carries nothing of that kind.
func (ans Answer) Value(kind string) (value float64, chosen, ok bool) { return usable(ans, kind) }

// Item is one parsed log line: the item Jev answered, when, and its answers.
type Item struct {
	ID      string
	Time    time.Time
	Model   string
	Answers map[QuestionID]Answer
	// Refs holds the adapter's own values from the line — the landing adapter
	// keeps the rig and head its labels are keyed by — and the scorecard never
	// reads them.
	Refs map[string]string
	// Context is a value per question to tally beside that question's block,
	// such as the agent a seat ran on. It is printed, never scored or labeled,
	// and it counts only the lines that gave the question a usable answer.
	Context map[QuestionID]string
}

// readableQuestionSet reports whether a line logged under question set version
// is one the scorecard can compare with the battery's current set. A battery
// drops a question by raising its version and leaving that question out, so an
// older line asked the remaining questions in the same words and its answers
// still compare. A set newer than current is one this scorecard has not been
// taught, and set 0 predates every battery.
func readableQuestionSet(version, current int) bool {
	return version >= 1 && version <= current
}

// Labeling is what an adapter finds for one item: each question's label, and
// the rule that set them, which the --json report names for the line.
type Labeling struct {
	// Labeled is false for an item no rule labels, which counts as logged but
	// not labeled. Labels and Rule are unset then.
	Labeled bool
	// Labels is each question's label. A question absent from it is not
	// labeled on this item.
	Labels map[QuestionID]bool
	// Rule is the stable name of the rule that set Labels.
	Rule string
	// Source names where the label came from, such as a polecat's review or
	// the overseer's, so the report counts them apart. An adapter with one
	// source leaves it empty.
	Source string
}

// Adapter reads one battery's log and labels its items from the ground truth
// the battery is judged against.
type Adapter interface {
	// Battery names the battery; it is the --battery value.
	Battery() string
	// LogPath is the JSONL log the battery appends to.
	LogPath() string
	// Parse reads one log line, and an error marks the line as skipped.
	Parse(raw []byte) (Item, error)
	// Label returns item's labels. An error fails the whole report.
	Label(ctx context.Context, item Item) (Labeling, error)
}

// Expander is an Adapter whose log line answers more than one item, such as a
// drift line that answers each doc section it names. The scorecard reads such a
// line through ParseLines in place of Parse, and an error skips the whole line.
type Expander interface {
	ParseLines(raw []byte) ([]Item, error)
}

// Contextual is an Adapter that also reports context lines for the report:
// facts gathered while labeling that the per-question metrics do not carry.
// Build reads Context once, after labeling.
type Contextual interface {
	Context() []string
}

// Factory builds an adapter for the town at townRoot.
type Factory func(townRoot string) Adapter

// registry is written only from init functions, so reads need no lock.
var registry = map[string]registration{}

// registration is one battery's factory and whether the battery listings leave
// it out.
type registration struct {
	factory  Factory
	testOnly bool
}

// Register makes a battery available to the scorecard under name. It panics on
// a duplicate name, which is a programming error.
func Register(name string, f Factory) {
	addRegistration(name, registration{factory: f})
}

// RegisterTestOnly makes a battery Open can build by name, but that Registered
// leaves out, so the unknown-battery error and the dashboard's per-battery reads
// never name it. It is for a battery that reads fixtures no real one writes.
func RegisterTestOnly(name string, f Factory) {
	addRegistration(name, registration{factory: f, testOnly: true})
}

func addRegistration(name string, r registration) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("scorecard: battery %q registered twice", name))
	}
	registry[name] = r
}

// Open builds the adapter registered under name for the town at townRoot. An
// unknown name is an error that lists the registered batteries.
func Open(name, townRoot string) (Adapter, error) {
	r, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown battery %q; registered batteries: %s", name, strings.Join(Registered(), ", "))
	}
	return r.factory(townRoot), nil
}

// Registered lists the registered battery names in name order, leaving out the
// test-only ones.
func Registered() []string {
	names := make([]string, 0, len(registry))
	for name, r := range registry {
		if !r.testOnly {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
