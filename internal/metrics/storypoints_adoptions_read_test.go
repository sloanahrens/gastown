package metrics

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadStoryPointsAdoptions: the adoptions log reads back in order with the check's fields;
// no file is no adoption, and a log that cannot be read or does not parse is an error naming the
// path, never an empty log (gt-q2rrvb, the reader of the week-after check).
func TestReadStoryPointsAdoptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "adoptions.jsonl")
	env := Env{ReadFile: os.ReadFile}

	t.Run("an absent log is no adoption", func(t *testing.T) {
		t.Parallel()
		got, err := readStoryPointsAdoptions(env, filepath.Join(dir, "absent.jsonl"))
		if err != nil || len(got) != 0 {
			t.Errorf("read = %v, %v; want no adoption and no error", got, err)
		}
	})

	t.Run("an adopted and a checked line read back in order", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "adoptions.jsonl")
		body := `{"event":"adopted","at":"2026-11-10T06:00:00Z","version":"v2","previous":"weights.v1.json"}` + "\n" +
			`{"event":"checked","at":"2026-11-17T06:00:00Z","adopted_at":"2026-11-10T06:00:00Z","outcome":"reverted","reason":"worse"}` + "\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := readStoryPointsAdoptions(env, p)
		if err != nil || len(got) != 2 {
			t.Fatalf("read = %v, %v; want two lines", got, err)
		}
		adoptedAt := time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC)
		if a := got[0]; a.Event != "adopted" || !a.At.Equal(adoptedAt) || a.Version != "v2" || a.Previous != "weights.v1.json" {
			t.Errorf("first = %+v, want the adoption", a)
		}
		if c := got[1]; c.Event != "checked" || c.Adopted == nil || !c.Adopted.Equal(adoptedAt) || c.Outcome != "reverted" || c.Reason != "worse" {
			t.Errorf("second = %+v, want the check with its adoption time, outcome and reason", c)
		}
	})

	t.Run("an unreadable file is an error naming the path", func(t *testing.T) {
		t.Parallel()
		failing := Env{ReadFile: func(string) ([]byte, error) { return nil, errors.New("disk gone") }}
		_, err := readStoryPointsAdoptions(failing, path)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "disk gone") {
			t.Errorf("err = %v, want the path and the cause", err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, must not read as an absent log", err)
		}
	})

	t.Run("a malformed line is an error naming the path", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "adoptions.jsonl")
		if err := os.WriteFile(p, []byte(`{"event":"adopted","at":"2026-11-10T06:00:00Z"}`+"\n{not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readStoryPointsAdoptions(env, p)
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("err = %v, want an error naming %s", err, p)
		}
	})
}
