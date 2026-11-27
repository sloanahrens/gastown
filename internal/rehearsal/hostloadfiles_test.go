package rehearsal

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRehearsalHostLoadFiles reads a window inside one day and one across
// midnight, with every file written after the reader is made, and refuses a
// missing or unreadable day file without serving bytes.
func TestRehearsalHostLoadFiles(t *testing.T) {
	t.Parallel()
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, loadZone) }
	for _, tt := range []struct {
		name      string
		from, now time.Time
		files     map[string]string
		want      string
		fail      string
	}{
		{"a window inside one day", at(20, 16), at(20, 17), map[string]string{"2026-11-20.log": "a\n"}, "a\n", ""},
		{"a window across midnight", at(20, 23), at(21, 1), map[string]string{"2026-11-20.log": "a\n", "2026-11-21.log": "b\n"}, "a\nb\n", ""},
		{"now's date is taken in from's zone", at(20, 23), at(21, 20).UTC(), map[string]string{"2026-11-20.log": "a\n", "2026-11-21.log": "b\n"}, "a\nb\n", ""},
		{"a missing day file", at(20, 23), at(21, 1), map[string]string{"2026-11-20.log": "a\n"}, "", "2026-11-21.log"},
		{"a day file that cannot be read", at(20, 23), at(21, 1), map[string]string{"2026-11-20.log": "a\n", "2026-11-21.log/": ""}, "", "2026-11-21.log"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			r := HostLoadReader(dir, tt.from, func() time.Time { return tt.now })
			for name, text := range tt.files {
				path := filepath.Join(dir, name)
				if strings.HasSuffix(name, "/") {
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := io.ReadAll(r)
			if tt.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tt.fail) || len(got) != 0 {
					t.Fatalf("read = %q, %v: want an error naming %s and no bytes", got, err, tt.fail)
				}
				if n, again := r.Read(make([]byte, 8)); n != 0 || again == nil || again.Error() != err.Error() {
					t.Fatalf("read after the error = %d, %v: want 0 and the same error", n, again)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Fatalf("read = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
