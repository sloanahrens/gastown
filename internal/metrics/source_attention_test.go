package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
)

// The fixtures below are the shapes the daemon and `gt attention ack` write,
// with the keys, rigs and summaries replaced. The ack lines carry the
// dispositions the ack command records; one line has none, as every line
// written before dispositions existed does.

const attentionTestTown = "/town"

const attentionStateFixture = `{"updated":"2026-10-24T13:04:02Z","items":[` +
	`{"key":"red-main:gastown:internal/cmd","kind":"red-main","severity":"high","rig":"gastown","summary":"<summary>","first_seen":"2026-10-24T12:30:00Z","last_seen":"2026-10-24T13:04:00Z"},` +
	`{"key":"queue-stuck:gastown","kind":"queue-stuck","severity":"high","rig":"gastown","summary":"<summary>","first_seen":"2026-10-24T12:40:00Z","last_seen":"2026-10-24T13:04:00Z"},` +
	`{"key":"risk:gt-a1:6cf8b456","kind":"risk-path","severity":"low","bead":"gt-a1","summary":"<summary>","first_seen":"2026-10-24T12:45:00Z","last_seen":"2026-10-24T13:04:00Z","acked_at":"2026-10-24T12:50:00Z"},` +
	`{"key":"future:gastown","kind":"future-kind","severity":"low","summary":"<summary>","first_seen":"2026-10-24T12:45:00Z","last_seen":"2026-10-24T13:04:00Z"}]}`

// attentionAcksFixture covers each disposition, an ack outside the hour, a
// line with no disposition, a disposition this source does not know, a line
// that will not parse and a line with no timestamp.
const attentionAcksFixture = `{"ts":"2026-10-24T11:00:00Z","key":"old:hq-a2","kind":"escalation","rule":"","disposition":"noise"}
{"ts":"2026-10-24T12:45:00Z","key":"esc:hq-a1","kind":"escalation","rule":"escalation","disposition":"needed-human"}
{"ts":"2026-10-24T12:50:00Z","key":"red-main:gastown","kind":"red-main","rule":"","disposition":"fixed-by-overseer"}
{"ts":"2026-10-24T12:55:00Z","key":"queue-stuck:gastown","kind":"queue-stuck","rule":"","disposition":"noise"}
{"ts":"2026-10-24T12:58:00Z","key":"legacy:gastown","kind":"queue-stuck","rule":""}
{"ts":"2026-10-24T12:59:00Z","key":"odd:gastown","kind":"queue-stuck","rule":"","disposition":"tomorrow-thing"}
not json
{"key":"no-ts:hq-a3","kind":"escalation","rule":"","disposition":"noise"}
`

// attentionTestEnv serves the named files and answers Tail with the byte bound
// the attention source is expected to use.
func attentionTestEnv(t *testing.T, files map[string]string, errs map[string]error) Env {
	t.Helper()
	env := envAt(fixedNow())
	env.Town = attentionTestTown
	env.ReadFile = func(path string) ([]byte, error) {
		if err := errs[path]; err != nil {
			return nil, err
		}
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	env.Tail = func(path string, maxBytes int64) ([]byte, error) {
		if maxBytes != sourceLogTailBytes {
			t.Errorf("Tail(%s) read %d bytes, want the %d-byte bound", path, maxBytes, sourceLogTailBytes)
		}
		if err := errs[path]; err != nil {
			return nil, err
		}
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	return env
}

func TestSourceAttentionOpenByKindAndAcksByDisposition(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		attention.StatePath(attentionTestTown):       attentionStateFixture,
		attention.AcksHistoryPath(attentionTestTown): attentionAcksFixture,
	}
	got, err := attentionSource{}.Collect(context.Background(), attentionTestEnv(t, files, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"open_total":                3, // the acked item is not open
		"open_red_main":             1,
		"open_queue_stuck":          1,
		"open_risk_path":            0, // acked
		"open_unknown_kind":         1, // kind future-kind
		"acks_1h":                   5,
		"acks_needed_human_1h":      1,
		"acks_fixed_by_overseer_1h": 1,
		"acks_noise_1h":             1,
		"acks_unlabelled_1h":        1,
		"acks_other_1h":             1,
		"acks_unparsed_lines":       2,
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %v, want %v", name, got[name], value)
		}
	}
	// Every kind attention.Kinds names has its own zeroed column, so a kind
	// that no item carries reads as 0 and not as an absent key. Two kinds must
	// not share one column, or one kind's count would land in another's.
	columns := map[string]attention.Kind{}
	for _, kind := range attention.Kinds() {
		column := attentionOpenColumn(kind)
		if other, taken := columns[column]; taken {
			t.Errorf("kinds %q and %q share column %s", other, kind, column)
		}
		columns[column] = kind
		if _, ok := got[column]; !ok {
			t.Errorf("no column for kind %q", kind)
		}
	}
}

// A state.json the daemon has not written yet, or one that will not parse, is
// a null group: an unwritten queue is not an empty one, and no column reads as
// a count of nothing. The source returns an error rather than panicking.
func TestSourceAttentionMissingOrMalformedStateIsANilGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string]string
		errs  map[string]error
	}{
		{name: "missing", files: nil},
		{name: "malformed", files: map[string]string{attention.StatePath(attentionTestTown): `{"items":`}},
		{name: "no state document", files: map[string]string{attention.StatePath(attentionTestTown): `null`}},
		{name: "unreadable", errs: map[string]error{attention.StatePath(attentionTestTown): fs.ErrPermission}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := attentionSource{}.Collect(context.Background(), attentionTestEnv(t, tc.files, tc.errs))
			if err == nil {
				t.Fatalf("Collect = %v, want an error so the group is null", got)
			}
			if got != nil {
				t.Fatalf("Collect = %v, want a nil group", got)
			}
		})
	}
}

// There is no acks-history.jsonl until the first ack, so a town that has not
// acked yet is an hour with no acks, not a group with no reading.
func TestSourceAttentionMissingAcksHistoryIsAnHourWithNoAcks(t *testing.T) {
	t.Parallel()
	files := map[string]string{attention.StatePath(attentionTestTown): attentionStateFixture}
	got, err := attentionSource{}.Collect(context.Background(), attentionTestEnv(t, files, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, column := range []string{
		"acks_1h", "acks_needed_human_1h", "acks_fixed_by_overseer_1h",
		"acks_noise_1h", "acks_unlabelled_1h", "acks_other_1h", "acks_unparsed_lines",
	} {
		if got[column] != 0 {
			t.Errorf("%s = %v, want 0 with no history", column, got[column])
		}
	}
	if got["open_total"] != 3 {
		t.Errorf("open_total = %v, want the state still counted", got["open_total"])
	}
}

// An ack history the process cannot read is an error: a permission failure is
// not a town that made no acks.
func TestSourceAttentionUnreadableAcksHistoryIsAnError(t *testing.T) {
	t.Parallel()
	files := map[string]string{attention.StatePath(attentionTestTown): attentionStateFixture}
	errs := map[string]error{attention.AcksHistoryPath(attentionTestTown): fs.ErrPermission}
	got, err := attentionSource{}.Collect(context.Background(), attentionTestEnv(t, files, errs))
	if err == nil {
		t.Fatalf("Collect = %v, want an error for an unreadable history", got)
	}
}

// The rotated log holds acks that fell out of the current file, and the hour
// window reads both: a recent ack there still counts.
func TestSourceAttentionReadsTheRotatedAcksHistory(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		attention.StatePath(attentionTestTown):              attentionStateFixture,
		attention.RotatedAcksHistoryPath(attentionTestTown): `{"ts":"2026-10-24T12:30:00Z","key":"esc:hq-a9","kind":"escalation","rule":"","disposition":"noise"}` + "\n",
	}
	got, err := attentionSource{}.Collect(context.Background(), attentionTestEnv(t, files, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["acks_1h"] != 1 || got["acks_noise_1h"] != 1 {
		t.Errorf("acks_1h = %v, acks_noise_1h = %v, want 1 and 1 from the rotated log", got["acks_1h"], got["acks_noise_1h"])
	}
}

// Every value the two groups write is a number, a bool or nil. No item key,
// summary, title or bead id reaches a column, so a reader that expects counts
// cannot be handed a queue's text.
func TestSourceAttentionAndConfigColumnsCarryNoItemText(t *testing.T) {
	t.Parallel()
	const text = "landing queue silent and a title no column may carry"
	state := `{"updated":"2026-10-24T13:04:02Z","items":[{"key":"` + text + `","kind":"queue-stuck","severity":"high","summary":"` + text + `","first_seen":"2026-10-24T12:40:00Z","last_seen":"2026-10-24T13:04:00Z"}]}`
	attentionCols, err := attentionSource{}.Collect(context.Background(), attentionTestEnv(t, map[string]string{
		attention.StatePath(attentionTestTown): state,
	}, nil))
	if err != nil {
		t.Fatalf("attention Collect: %v", err)
	}
	settings, rigs, _, _ := configTestPaths()
	configCols, err := configSource{}.Collect(context.Background(), configTestEnv(t, map[string]string{
		settings:                configSettingsFixture,
		rigs:                    configRigsFixture("gastown"),
		configOmPath("gastown"): `{"threshold": 0.75}`,
	}, nil, configRigList(t, `[{"name": "gastown", "status": "operational"}]`)))
	if err != nil {
		t.Fatalf("config Collect: %v", err)
	}
	for _, group := range []struct {
		name string
		cols map[string]any
	}{{"attention", attentionCols}, {"config", configCols}} {
		for column, value := range group.cols {
			switch value.(type) {
			case nil, bool, int, int64, float64:
			default:
				t.Errorf("%s.%s = %#v (%T), want a number, a bool or nil", group.name, column, value, value)
			}
			if strings.Contains(fmt.Sprint(value), text) {
				t.Errorf("%s.%s carries the item's text", group.name, column)
			}
		}
	}
}

// A tail that hit the byte bound may start mid-line; that cut line is counted
// as unplaceable, not parsed as a record.
func TestAttentionTailLinesDropsACutFirstLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		data      string
		truncated bool
		want      []string
		wantCut   int
	}{
		{"whole read keeps every line", "one\ntwo\n", false, []string{"one", "two", ""}, 0},
		{"cut read drops the first line", "artial line\ntwo\n", true, []string{"two", ""}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, cut := attentionTailLines([]byte(tc.data), tc.truncated)
			if cut != tc.wantCut || !reflect.DeepEqual(lines, tc.want) {
				t.Errorf("attentionTailLines(%q, %t) = %q, %d; want %q, %d", tc.data, tc.truncated, lines, cut, tc.want, tc.wantCut)
			}
		})
	}
}
