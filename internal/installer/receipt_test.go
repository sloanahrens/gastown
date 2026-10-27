package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The lines below are what the script's python writes for the same record:
// keys sorted, ", " between them, null for a field it has nothing to say about,
// and whole-second UTC timestamps.
func TestReceiptLine(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 26, 19, 20, 36, 0, time.UTC)
	cases := []struct {
		name string
		in   receipt
		want string
	}{
		{
			name: "every field",
			in: receipt{
				ts:       ts,
				event:    "installed",
				commit:   "abc123",
				prev:     "def456",
				source:   "manual",
				reason:   "smoke-failed",
				mergedAt: time.Date(2026, 10, 26, 19, 10, 0, 0, time.UTC),
				start:    ts.Add(-12 * time.Second),
			},
			want: `{"commit": "abc123", "duration_s": 12, "event": "installed", ` +
				`"merged_at": "2026-10-26T19:10:00Z", "prev_commit": "def456", ` +
				`"reason": "smoke-failed", "source": "manual", "ts": "2026-10-26T19:20:36Z"}`,
		},
		{
			name: "an absent field is null",
			in:   receipt{ts: ts, event: "failed"},
			want: `{"commit": null, "duration_s": null, "event": "failed", "merged_at": null, ` +
				`"prev_commit": null, "reason": null, "source": null, "ts": "2026-10-26T19:20:36Z"}`,
		},
		{
			name: "a partial second is truncated, as int() is",
			in:   receipt{ts: ts, event: "noop", start: ts.Add(-1500 * time.Millisecond)},
			want: `{"commit": null, "duration_s": 1, "event": "noop", "merged_at": null, ` +
				`"prev_commit": null, "reason": null, "source": null, "ts": "2026-10-26T19:20:36Z"}`,
		},
		{
			name: "both timestamps are written in UTC",
			in: receipt{
				ts:       ts.In(time.FixedZone("elsewhere", 3600)),
				event:    "noop",
				mergedAt: time.Date(2026, 10, 26, 20, 10, 0, 0, time.FixedZone("elsewhere", 3600)),
			},
			want: `{"commit": null, "duration_s": null, "event": "noop", ` +
				`"merged_at": "2026-10-26T19:10:00Z", "prev_commit": null, "reason": null, ` +
				`"source": null, "ts": "2026-10-26T19:20:36Z"}`,
		},
		{
			name: "non-ASCII is escaped, as python does",
			in:   receipt{ts: ts, event: "failed", reason: "café & 日本"},
			want: `{"commit": null, "duration_s": null, "event": "failed", "merged_at": null, ` +
				`"prev_commit": null, "reason": "caf\u00e9 & \u65e5\u672c", "source": null, ` +
				`"ts": "2026-10-26T19:20:36Z"}`,
		},
		{
			name: "an astral character is a surrogate pair, as python writes it",
			in:   receipt{ts: ts, event: "failed", reason: "\U0001F600"},
			want: `{"commit": null, "duration_s": null, "event": "failed", "merged_at": null, ` +
				`"prev_commit": null, "reason": "\ud83d\ude00", "source": null, ` +
				`"ts": "2026-10-26T19:20:36Z"}`,
		},
		{
			name: "control characters use python's short escapes where it has them",
			in:   receipt{ts: ts, event: "failed", reason: "a\bb\fc\u0001d\u007fe"},
			want: `{"commit": null, "duration_s": null, "event": "failed", "merged_at": null, ` +
				`"prev_commit": null, "reason": "a\bb\fc\u0001d\u007fe", "source": null, ` +
				`"ts": "2026-10-26T19:20:36Z"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.line(); got != tc.want {
				t.Errorf("line() = %s\nwant      %s", got, tc.want)
			}
		})
	}
}

// appendReceipt creates the daemon directory the script's mkdir -p makes and
// adds a line without touching the ones already there.
func TestAppendReceipt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon", "install-receipts.jsonl")

	for _, line := range []string{`{"event": "noop"}`, `{"event": "installed"}`} {
		if err := appendReceipt(path, line); err != nil {
			t.Fatalf("appendReceipt(%q): %v", line, err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"event\": \"noop\"}\n{\"event\": \"installed\"}\n"
	if string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// A character the script's python writes as itself stays itself: json's HTML
// escaping would make the two writers' lines differ.
func TestReceiptLineDoesNotEscapeHTML(t *testing.T) {
	t.Parallel()
	rec := receipt{
		ts:     time.Date(2026, 10, 26, 19, 20, 36, 0, time.UTC),
		event:  "failed",
		reason: "build&sign",
	}

	if got := rec.line(); !strings.Contains(got, `"reason": "build&sign"`) {
		t.Errorf("line() = %s, want the reason written as itself", got)
	}
}
