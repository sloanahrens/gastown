package daemon

import (
	"fmt"
	"io"
	"log"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseWispID(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
	}{
		{
			name:   "standard wisp output",
			input:  "✓ Spawned wisp: gt-wisp-abc123 — Reap stale wisps",
			wantID: "gt-wisp-abc123",
		},
		{
			name:   "wisp ID with ANSI codes",
			input:  "\033[32m✓\033[0m Spawned wisp: \033[1mgt-wisp-xyz789\033[0m — Title",
			wantID: "gt-wisp-xyz789",
		},
		{
			name:   "empty output",
			input:  "",
			wantID: "",
		},
		{
			name:   "no wisp ID in output",
			input:  "Error: something went wrong",
			wantID: "",
		},
		{
			name:   "wisp ID at end of line",
			input:  "Created gt-wisp-def456",
			wantID: "gt-wisp-def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWispID(tt.input)
			if got != tt.wantID {
				t.Errorf("parseWispID(%q) = %q, want %q", tt.input, got, tt.wantID)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no ANSI", "hello", "hello"},
		{"color code", "\033[32mgreen\033[0m", "green"},
		{"bold", "\033[1mbold\033[0m", "bold"},
		{"multiple codes", "\033[32m✓\033[0m \033[1mtext\033[0m", "✓ text"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseChildrenJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "bare array",
			input:   `[{"id":"a","title":"Probe","status":"open"}]`,
			wantIDs: []string{"a"},
		},
		{
			name:    "map wrapper from bd show",
			input:   `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"},{"id":"hq-wisp-b","title":"Report","status":"open"}]}`,
			wantIDs: []string{"hq-wisp-a", "hq-wisp-b"},
		},
		{
			name:    "empty map wrapper",
			input:   `{"hq-wisp-root":[]}`,
			wantIDs: []string{},
		},
		{
			name:    "schema metadata with children",
			input:   `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}],"schema_version":1}`,
			wantIDs: []string{"hq-wisp-a"},
		},
		{
			name:    "schema metadata with empty children",
			input:   `{"hq-wisp-root":[],"schema_version":1}`,
			wantIDs: []string{},
		},
		{
			name:    "multiple child arrays are deterministic",
			input:   `{"hq-wisp-b":[{"id":"b-step","title":"Report","status":"open"}],"schema_version":1,"hq-wisp-a":[{"id":"a-step","title":"Probe","status":"open"}]}`,
			wantIDs: []string{"a-step", "b-step"},
		},
		{
			name:    "schema key is metadata even if array-valued",
			input:   `{"schema_version":[{"id":"metadata","title":"Ignore","status":"open"}],"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}]}`,
			wantIDs: []string{"hq-wisp-a"},
		},
		{
			name:    "empty array",
			input:   `[]`,
			wantIDs: []string{},
		},
		{
			name:    "empty input",
			input:   `   `,
			wantErr: true,
		},
		{
			name:    "malformed bare array",
			input:   `[`,
			wantErr: true,
		},
		{
			name:    "malformed object envelope",
			input:   `{"hq-wisp-root":[`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
		{
			name:    "malformed child array",
			input:   `{"hq-wisp-root":[{"id":1}],"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "non-array child payload",
			input:   `{"hq-wisp-root":1,"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "metadata only is not silent skip-all",
			input:   `{"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "empty object is not silent skip-all",
			input:   `{}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChildrenJSON(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			gotIDs := make([]string, 0, len(got))
			for _, child := range got {
				gotIDs = append(gotIDs, child.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("got child IDs %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// fakeDogBd simulates the subset of `bd show <root> --children --json` and
// `bd close <id> [--force] [--reason X]` that closeRemainingSteps depends on,
// modeling a dependency chain: closing an id fails with "blocked by open
// issues" until every id in blockedBy[id] has closed, unless --force is
// passed (which always succeeds, matching real bd semantics).
type fakeDogBd struct {
	rootID     string
	statuses   map[string]string   // id -> "open" | "closed"
	blockedBy  map[string][]string // id -> ids that must close first
	closeCalls []string            // every id `bd close` was invoked with, in call order
	forceCalls []string            // ids force-closed
}

func (f *fakeDogBd) run(args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakeDogBd: no args")
	}
	switch args[0] {
	case "show":
		return f.show(), nil
	case "close":
		return f.close(args[1:])
	default:
		return "", fmt.Errorf("fakeDogBd: unexpected command: %v", args)
	}
}

func (f *fakeDogBd) show() string {
	ids := make([]string, 0, len(f.statuses))
	for id := range f.statuses {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var sb strings.Builder
	fmt.Fprintf(&sb, "{%q:[", f.rootID)
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%q,"title":%q,"status":%q}`, id, id, f.statuses[id])
	}
	sb.WriteString("]}")
	return sb.String()
}

func (f *fakeDogBd) close(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakeDogBd: close missing id")
	}
	id := args[0]
	force := false
	for _, a := range args[1:] {
		if a == "--force" {
			force = true
		}
	}
	f.closeCalls = append(f.closeCalls, id)

	if !force {
		for _, blocker := range f.blockedBy[id] {
			if f.statuses[blocker] != "closed" {
				return "", fmt.Errorf("cannot close %s: blocked by open issues [%s]", id, blocker)
			}
		}
	} else {
		f.forceCalls = append(f.forceCalls, id)
	}
	f.statuses[id] = "closed"
	return "", nil
}

// TestCloseRemainingSteps_DrainsDependencyChainAcrossPasses is the adversarial
// case for gt-bygj: children are returned in an order where the blocked step
// is tried before its blocker (alphabetical "hq-wisp-c1" before
// "hq-wisp-c2"). A single pass over that order — the pre-fix behavior — closes
// c2 but leaves c1 permanently stranded (3 failed attempts, never retried).
// The fix must revisit c1 after c2 closes.
func TestCloseRemainingSteps_DrainsDependencyChainAcrossPasses(t *testing.T) {
	fake := &fakeDogBd{
		rootID: "hq-wisp-root",
		statuses: map[string]string{
			"hq-wisp-c1": "open",
			"hq-wisp-c2": "open",
		},
		blockedBy: map[string][]string{
			"hq-wisp-c1": {"hq-wisp-c2"},
		},
	}

	dm := &dogMol{
		rootID:  fake.rootID,
		stepIDs: make(map[string]string),
		logger:  log.New(io.Discard, "", 0),
		runBdFn: fake.run,
	}

	dm.closeRemainingSteps()

	for id, status := range fake.statuses {
		if status != "closed" {
			t.Errorf("expected %s closed, got status %q", id, status)
		}
	}
	if len(fake.forceCalls) != 0 {
		t.Errorf("expected no force-closes for a resolvable chain, got %v", fake.forceCalls)
	}
}

// TestCloseRemainingSteps_ForceClosesUnresolvableTail covers a dependency
// cycle (or a blocker outside the root's own children) that natural retries
// can never resolve. The fix must force-close the tail instead of leaving it
// HOOKED/open forever.
func TestCloseRemainingSteps_ForceClosesUnresolvableTail(t *testing.T) {
	fake := &fakeDogBd{
		rootID: "hq-wisp-root2",
		statuses: map[string]string{
			"hq-wisp-a": "open",
			"hq-wisp-b": "open",
		},
		blockedBy: map[string][]string{
			"hq-wisp-a": {"hq-wisp-b"},
			"hq-wisp-b": {"hq-wisp-a"},
		},
	}

	dm := &dogMol{
		rootID:  fake.rootID,
		stepIDs: make(map[string]string),
		logger:  log.New(io.Discard, "", 0),
		runBdFn: fake.run,
	}

	dm.closeRemainingSteps()

	for id, status := range fake.statuses {
		if status != "closed" {
			t.Errorf("expected %s force-closed, got status %q", id, status)
		}
	}
	if len(fake.forceCalls) != 2 {
		t.Errorf("expected both deadlocked children to be force-closed, got %v", fake.forceCalls)
	}
}

func TestDogMolGracefulDegradation(t *testing.T) {
	// A dogMol with empty rootID should be a no-op for all operations.
	dm := &dogMol{
		rootID:  "",
		stepIDs: make(map[string]string),
	}

	// These should not panic or error — graceful degradation.
	dm.closeStep("scan")
	dm.failStep("scan", "test failure")
	dm.close()
}
