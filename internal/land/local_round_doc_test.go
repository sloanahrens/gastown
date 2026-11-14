package land

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestLocalRoundDocStatesTheRules holds the local round design page to the rules
// the code enforces. The bound and the switch are built from the constant and
// the config names that enforce them, so a changed rule fails here until the
// page states it; the unavailable rule's sentence is pinned as the page writes
// it.
func TestLocalRoundDocStatesTheRules(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "local-round.md"))
	if err != nil {
		t.Fatalf("read the local round design page: %v", err)
	}
	doc := string(data)
	for _, want := range []struct {
		name   string
		phrase string
	}{
		{"the bound", wordForBound(t, localRoundsPerDay) + " per bead per day"},
		{"the switch", localRoundSwitchKey(t)},
		// The patrol fetches before Allow counts (internal/daemon/local_round.go),
		// so a fetch failure spends no round; the page must say so.
		{"a fetch failure is not a round", "A request whose head cannot be fetched is not a round"},
		// gt done's gate enforces this rule (internal/cmd/done_local_round.go),
		// and no land constant carries its sentence to build the row from.
		{"the unavailable rule", "An unavailable answer never blocks a submit"},
	} {
		t.Run(want.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(doc, want.phrase) {
				t.Errorf("docs/design/local-round.md does not state %q (%q)", want.name, want.phrase)
			}
		})
	}
}

// wordForBound is n in words for the page's bound sentence, and fails the test
// for a bound it has no word for so a raised bound cannot leave the page's
// number unchecked.
func wordForBound(t *testing.T, n int) string {
	t.Helper()
	words := [...]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n < 0 || n >= len(words) {
		t.Fatalf("localRoundsPerDay = %d has no word here; extend wordForBound and state the new bound on the design page", n)
	}
	return words[n]
}

// localRoundSwitchKey is the daemon setting the page names: the daemon file's
// name, then the JSON names the daemon config reads, so a renamed key fails
// here until the page names the new one.
func localRoundSwitchKey(t *testing.T) string {
	t.Helper()
	return "daemon." + jsonFieldName(t, config.DaemonPatrolConfig{}, "LocalRoundSetting") +
		"." + jsonFieldName(t, config.LocalRoundConfig{}, "Enabled")
}

// jsonFieldName is the JSON name of v's field, which is the key a daemon config
// file sets it by.
func jsonFieldName(t *testing.T, v any, field string) string {
	t.Helper()
	f, ok := reflect.TypeOf(v).FieldByName(field)
	if !ok {
		t.Fatalf("%T has no field %s", v, field)
	}
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		t.Fatalf("%T.%s has no JSON name", v, field)
	}
	return name
}
