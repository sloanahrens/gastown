package cmd

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// townConfigSet is one ConfigSet call configureTownBeadsWith made.
type townConfigSet struct {
	key   string
	value string
}

// townBeadsRecorder is a bd client for configureTownBeadsWith: it records each
// ConfigSet call and returns the error errs holds for that key, if any.
type townBeadsRecorder struct {
	calls []townConfigSet
	errs  map[string]error
}

func (r *townBeadsRecorder) ConfigSet(key, value string) error {
	r.calls = append(r.calls, townConfigSet{key: key, value: value})
	return r.errs[key]
}

// A new town gets explicit routing and nothing else: in particular the events
// journal stays off, so gt install does not turn journaling on behind the
// operator's back.
func TestConfigureTownBeadsWithSetsOnlyRoutingMode(t *testing.T) {
	t.Parallel()
	rec := &townBeadsRecorder{}
	var out bytes.Buffer
	configureTownBeadsWith(rec, &out)

	want := []townConfigSet{{key: "routing.mode", value: "explicit"}}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("configureTownBeadsWith set %v, want exactly %v", rec.calls, want)
	}
	if out.Len() != 0 {
		t.Errorf("configureTownBeadsWith wrote %q with no error, want nothing", out.String())
	}
}

// A refused routing.mode is reported to the given writer and the function
// returns normally: the town's database already exists, so install continues.
func TestConfigureTownBeadsWithWarnsOnRoutingModeError(t *testing.T) {
	t.Parallel()
	rec := &townBeadsRecorder{errs: map[string]error{
		"routing.mode": errors.New("config set refused"),
	}}
	var out bytes.Buffer
	configureTownBeadsWith(rec, &out)

	if len(rec.calls) != 1 || rec.calls[0].key != "routing.mode" {
		t.Errorf("configureTownBeadsWith tried %v, want the one routing.mode call", rec.calls)
	}
	got := out.String()
	if !strings.Contains(got, "Could not set routing.mode") {
		t.Errorf("configureTownBeadsWith wrote %q, want a routing.mode warning", got)
	}
	if !strings.Contains(got, "config set refused") {
		t.Errorf("configureTownBeadsWith wrote %q, want the bd error text", got)
	}
	if strings.Contains(got, beads.EventsJournalKey) {
		t.Errorf("configureTownBeadsWith wrote %q, want no events journal message", got)
	}
}
