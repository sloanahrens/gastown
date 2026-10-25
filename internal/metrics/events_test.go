package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// regimeEnv serves one regime-events file (or the error) at the town's path.
func regimeEnv(content string, readErr error) Env {
	env := envAt(fixedNow())
	env.Town = "/town"
	env.ReadFile = func(path string) ([]byte, error) {
		if path != filepath.Join("/town", "settings", "regime-events.json") {
			return nil, errors.New("read of an unexpected path: " + path)
		}
		if readErr != nil {
			return nil, readErr
		}
		return []byte(content), nil
	}
	return env
}

// regimeEventsJSON builds a schema-1 file of n one-line events.
func regimeEventsJSON(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"date":"2026-10-10","what":"change %d"}`, i)
	}
	return `{"v":1,"events":[` + strings.Join(entries, ",") + `]}`
}

// noEvents checks a result that means no event is known: empty, non-nil, and
// marshalling as [] so a panel without marks still gets an array.
func noEvents(t *testing.T, events []RegimeEvent) {
	t.Helper()
	if events == nil || len(events) != 0 {
		t.Fatalf("events = %#v; want an empty non-nil list", events)
	}
	data, err := json.Marshal(events)
	if err != nil || string(data) != "[]" {
		t.Fatalf("marshal = %s, %v; want []", data, err)
	}
}

func TestReadRegimeEvents(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 130)
	tests := []struct {
		name    string
		content string
		readErr error
		wantErr string
		check   func(*testing.T, []RegimeEvent)
	}{
		{
			name: "a valid file comes back sorted by date, then in file order",
			content: `{"v":1,"events":[
				{"date":"2026-10-20","what":"  gate raised to 3  "},
				{"date":"2026-10-01","what":"flash seats replaced by Sonnet seats","confirmed":true},
				{"date":"2026-10-20","what":"second change on the same day"}
			]}`,
			check: func(t *testing.T, events []RegimeEvent) {
				want := []RegimeEvent{
					{Date: "2026-10-01", What: "flash seats replaced by Sonnet seats", Confirmed: true},
					{Date: "2026-10-20", What: "gate raised to 3"},
					{Date: "2026-10-20", What: "second change on the same day"},
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("events = %#v; want %#v", events, want)
				}
			},
		},
		{name: "a missing file is no events and no error", readErr: fs.ErrNotExist, check: noEvents},
		{name: "an empty events array is no events", content: `{"v":1,"events":[]}`, check: noEvents},
		{
			name:    "a what over 120 runes is cut",
			content: `{"v":1,"events":[{"date":"2026-10-10","what":"` + long + `"}]}`,
			check: func(t *testing.T, events []RegimeEvent) {
				want := string([]rune(long)[:120])
				if got := events[0].What; got != want || len([]rune(got)) != 120 {
					t.Fatalf("what = %q (%d runes); want 120 runes ending at %q", got, len([]rune(got)), want)
				}
			},
		},
		{
			name:    "sixty-four events is the limit",
			content: regimeEventsJSON(64),
			check: func(t *testing.T, events []RegimeEvent) {
				if len(events) != 64 {
					t.Fatalf("64 events = %d; want 64", len(events))
				}
			},
		},
		{name: "sixty-five events", content: regimeEventsJSON(65), wantErr: "65"},
		{name: "an unreadable file", readErr: fs.ErrPermission, wantErr: "permission"},
		{name: "malformed JSON", content: `{"v":1,"events":[`, wantErr: "decoding"},
		{name: "an unknown top-level field", content: `{"v":1,"events":[],"extra":1}`, wantErr: "unknown field"},
		{name: "an unknown event field", content: `{"v":1,"events":[{"date":"2026-10-10","what":"x","source":"log"}]}`, wantErr: "unknown field"},
		{name: "another schema version", content: `{"v":2,"events":[]}`, wantErr: "schema version 2"},
		{name: "no version", content: `{"events":[]}`, wantErr: "schema version 0"},
		{name: "data after the object", content: `{"v":1,"events":[]} {"v":1}`, wantErr: "data after"},
		{name: "a day that is not on the calendar", content: `{"v":1,"events":[{"date":"2026-10-32","what":"x"}]}`, wantErr: "date"},
		{name: "a date in another form", content: `{"v":1,"events":[{"date":"10/10/2026","what":"x"}]}`, wantErr: "date"},
		{name: "a date that is missing", content: `{"v":1,"events":[{"what":"x"}]}`, wantErr: "date"},
		{name: "an empty what", content: `{"v":1,"events":[{"date":"2026-10-10","what":"   "}]}`, wantErr: "empty"},
		{name: "a what that is not a string", content: `{"v":1,"events":[{"date":"2026-10-10","what":7}]}`, wantErr: "decoding"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events, err := ReadRegimeEvents(regimeEnv(tc.content, tc.readErr))
			if tc.wantErr != "" {
				if err == nil || events != nil {
					t.Fatalf("ReadRegimeEvents = %v, %v; want an error and no events", events, err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadRegimeEvents: %v", err)
			}
			tc.check(t, events)
		})
	}
}
