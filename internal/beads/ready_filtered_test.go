package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// filteredBoardEnvelope is the shape bd prints under BD_JSON_ENVELOPE=1 for a
// ready page: a data array and the pagination verdict. It is deliberately the
// envelope runReadyCLI asks for, not machine mode's (which carries
// contract_version): ReadyFiltered reads this shape (gt-p85pw.7).
func filteredBoardEnvelope(returned, total int, truncated bool) string {
	var items []string
	for i := 0; i < returned; i++ {
		items = append(items, fmt.Sprintf(`{"id":"gt-page-%d","title":"t","status":"open","priority":2,"issue_type":"task"}`, i))
	}
	return fmt.Sprintf(`{"schema_version":1,"data":[%s],"pagination":{"returned":%d,"total":%d,"truncated":%t}}`,
		strings.Join(items, ","), returned, total, truncated)
}

// filteredReadyOptions is a representative filtered query: an unassigned
// board with the caller's own label and type exclusions, uncapped. It is not
// a named caller's query — the spec dispatcher's options are its own, pinned
// in internal/cmd (gt-p85pw.7).
func filteredReadyOptions() ReadyOptions {
	return ReadyOptions{
		Unassigned:    true,
		ExcludeLabels: []string{"gt:skipped", "needs-human"},
		ExcludeTypes:  []string{"epic", "wisp"},
		Limit:         0,
	}
}

// TestReadyFiltered_SendsReadyArgs pins the argv and the envelope opt-in
// ReadyFiltered sends: ReadyArgs(opts) exactly, through the BD_JSON_ENVELOPE=1
// path, so the pagination verdict Ready's parser reads is present.
func TestReadyFiltered_SendsReadyArgs(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: filteredBoardEnvelope(2, 2, false)} })
	opts := filteredReadyOptions()
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(opts)
	if err != nil {
		t.Fatalf("ReadyFiltered: %v", err)
	}
	if len(issues) != 2 || issues[0].ID != "gt-page-0" {
		t.Fatalf("ReadyFiltered = %v, want the two-page rows", issues)
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %q, want one bd ready", rec.argvs())
	}
	if want := ReadyArgs(opts); !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %q, want %q", calls[0].args, want)
	}
	if want := []string{"ready", "--json", "--unassigned", "--exclude-label", "gt:skipped,needs-human", "--exclude-type", "epic,wisp", "--limit", "0"}; !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %q, want the dispatcher's fixed flag order %q", calls[0].args, want)
	}
	if v, ok := lastEnvValue(calls[0].env, "BD_JSON_ENVELOPE"); !ok || v != "1" {
		t.Errorf("BD_JSON_ENVELOPE = %q (set %t), want 1 so bd answers with the pagination envelope", v, ok)
	}
}

// TestReadyFilteredCarriesThePinnedReadOnlyPolicy pins what a pinned
// ReadyFiltered query sends bd, the policy the spec dispatcher's board query
// relies on: the wrapper's own database with an inherited BEADS_DIR stripped,
// the ready envelope opt-in, read-only mode, and bd's stale-read bypass only
// when the installed bd takes it.
func TestReadyFilteredCarriesThePinnedReadOnlyPolicy(t *testing.T) {
	rigPath := t.TempDir()
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	t.Setenv("BEADS_DIR", filepath.Join(t.TempDir(), "elsewhere", ".beads"))

	tests := []struct {
		name        string
		allowStale  bool
		wantAllowed bool
	}{
		{"bd takes --allow-stale", true, true},
		{"bd rejects --allow-stale", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(func([]string) reply { return reply{stdout: filteredBoardEnvelope(0, 0, false)} })
			rec.allowStale = tt.allowStale
			b := NewPinned(beadsDir)
			b.exec = rec.exec
			if _, err := b.ReadyFiltered(filteredReadyOptions()); err != nil {
				t.Fatalf("ReadyFiltered: %v", err)
			}
			calls := rec.calls()
			if len(calls) != 1 {
				t.Fatalf("calls = %q, want one bd ready", rec.argvs())
			}
			call := calls[0]

			if got := envValues(call.env, "BEADS_DIR"); !slices.Equal(got, []string{ResolveBeadsDir(beadsDir)}) {
				t.Errorf("BEADS_DIR = %q, want exactly the rig's resolved database %q", got, ResolveBeadsDir(beadsDir))
			}
			if v, ok := lastEnvValue(call.env, "BD_JSON_ENVELOPE"); !ok || v != "1" {
				t.Errorf("BD_JSON_ENVELOPE = %q (set %t), want 1", v, ok)
			}
			if v, ok := lastEnvValue(call.env, "BD_READONLY"); !ok || v != "true" {
				t.Errorf("BD_READONLY = %q (set %t), want a pinned ready read to be read-only", v, ok)
			}
			want := ReadyArgs(filteredReadyOptions())
			if tt.wantAllowed {
				want = append([]string{"--allow-stale"}, want...)
			}
			if !slices.Equal(call.args, want) {
				t.Errorf("argv = %q, want %q", call.args, want)
			}
		})
	}
}

// envValues returns every value of key in env, in order, so a test can tell an
// inherited entry from the one the wrapper appended.
func envValues(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if k, v, found := strings.Cut(kv, "="); found && k == key {
			out = append(out, v)
		}
	}
	return out
}

// TestReadyFiltered_CutWholeBoardStillReturnsTheBoard: --limit 0 asks for
// every row, so a page bd cuts anyway is a partial board with the sentinel
// saying so, not a refusal. The spec dispatcher reads the page and lets the
// tick proceed on what it got (gt-p85pw.7).
func TestReadyFiltered_CutWholeBoardStillReturnsTheBoard(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: filteredBoardEnvelope(100, 373, true)} })
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(filteredReadyOptions())
	var truncated *ErrReadyTruncated
	if !errors.As(err, &truncated) {
		t.Fatalf("ReadyFiltered(limit 0) err = %v, want *ErrReadyTruncated", err)
	}
	if len(issues) != 100 {
		t.Errorf("ReadyFiltered(limit 0) = %d issues, want the 100-row partial board with its sentinel", len(issues))
	}
	if truncated.Cap != bdReadyDefaultLimit || truncated.TrueCount != 373 {
		t.Errorf("sentinel = %+v, want bd's default page cap and the 373-row board", *truncated)
	}
}

// TestReadyFiltered_APageRequestCarriesTheTruncation: a positive Limit is a
// page request, so the cut page comes back with the sentinel Ready uses and
// the caller's own limit as its cap.
func TestReadyFiltered_APageRequestCarriesTheTruncation(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: filteredBoardEnvelope(5, 9, true)} })
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(ReadyOptions{Limit: 5})
	var truncated *ErrReadyTruncated
	if !errors.As(err, &truncated) {
		t.Fatalf("ReadyFiltered(limit 5) err = %v, want *ErrReadyTruncated", err)
	}
	if truncated.Cap != 5 || truncated.Found != 5 || truncated.TrueCount != 9 {
		t.Errorf("sentinel = %+v, want cap 5, 5 found and 9 total", *truncated)
	}
	if len(issues) != 5 {
		t.Errorf("ReadyFiltered(limit 5) = %d issues, want the 5-row page", len(issues))
	}
}

// TestReadyFiltered_ReadsTheAnswersBdCanGive pins the decode branches: an
// uncapped envelope, the bare array an older bd prints (which carries no
// pagination verdict), empty output — a quiet board, not a failure — and a
// null data key beside a populated issues key, which is the older key's rows.
func TestReadyFiltered_ReadsTheAnswersBdCanGive(t *testing.T) {
	t.Parallel()
	row := `{"id":"gt-a","title":"t","status":"open","priority":2,"issue_type":"task"}`
	tests := []struct {
		name string
		out  string
		want int
	}{
		{"uncapped envelope", `{"schema_version":1,"data":[` + row + `]}`, 1},
		{"legacy bare array", `[` + row + `]`, 1},
		{"empty output", "", 0},
		{"null data beside issues", `{"data":null,"issues":[` + row + `]}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: tt.out} })
			issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(filteredReadyOptions())
			if err != nil {
				t.Fatalf("ReadyFiltered: %v", err)
			}
			if len(issues) != tt.want {
				t.Errorf("ReadyFiltered = %d issues, want %d", len(issues), tt.want)
			}
		})
	}
}
