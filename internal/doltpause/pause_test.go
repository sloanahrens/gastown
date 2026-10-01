package doltpause

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func writeMarker(t *testing.T, until time.Time) string {
	t.Helper()
	town := t.TempDir()
	if err := Write(town, Marker{Actor: "deacon", Reason: "weekly gc", Until: until, Since: t0}); err != nil {
		t.Fatal(err)
	}
	return town
}

func TestCurrent_ActiveMarker(t *testing.T) {
	t.Parallel()
	town := writeMarker(t, t0.Add(time.Hour))
	m := Current(town, t0)
	if m == nil {
		t.Fatal("Current = nil, want the marker")
	}
	if m.Actor != "deacon" || m.Reason != "weekly gc" || !m.Until.Equal(t0.Add(time.Hour)) {
		t.Errorf("marker = %+v", m)
	}
	if !strings.HasPrefix(m.Message(), "Dolt paused by deacon until ") || !strings.HasSuffix(m.Message(), ": weekly gc") {
		t.Errorf("Message = %q", m.Message())
	}
}

func TestCurrent_LapsedMarkerIsAbsent(t *testing.T) {
	t.Parallel()
	town := writeMarker(t, t0.Add(time.Minute))
	if m := Current(town, t0.Add(time.Minute)); m != nil {
		t.Errorf("Current at until = %+v, want nil", m)
	}
}

func TestCurrent_MissingAndMalformed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if m := Current(town, t0); m != nil {
		t.Errorf("no marker: Current = %+v", m)
	}
	if m := Current("", t0); m != nil {
		t.Errorf("empty town: Current = %+v", m)
	}
	if err := os.MkdirAll(town+"/daemon", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"not json", `{"actor":"x","reason":"y"}`} {
		if err := os.WriteFile(Path(town), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if m := Current(town, t0); m != nil {
			t.Errorf("%q: Current = %+v, want nil", body, m)
		}
		if _, err := Read(town); err == nil {
			t.Errorf("%q: Read err = nil, want parse error", body)
		}
	}
}

func TestWrite_RequiresFields(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, m := range []Marker{
		{Reason: "r", Until: t0},
		{Actor: "a", Until: t0},
		{Actor: "a", Reason: "r"},
	} {
		if err := Write(town, m); err == nil {
			t.Errorf("Write(%+v) = nil, want error", m)
		}
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	town := writeMarker(t, t0.Add(time.Hour))
	if had, err := Remove(town); !had || err != nil {
		t.Fatalf("Remove = %v, %v; want true, nil", had, err)
	}
	if had, err := Remove(town); had || err != nil {
		t.Fatalf("second Remove = %v, %v; want false, nil", had, err)
	}
}

func TestExplain(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection refused")
	town := writeMarker(t, t0.Add(time.Hour))

	err := Explain(town, t0, cause)
	var pe *Error
	if !errors.As(err, &pe) || !errors.Is(err, cause) {
		t.Fatalf("Explain = %v, want *Error wrapping cause", err)
	}
	if !strings.HasPrefix(err.Error(), "Dolt paused by deacon") {
		t.Errorf("Error() = %q", err.Error())
	}
	if again := Explain(town, t0, err); again != err {
		t.Errorf("Explain re-wrapped an *Error")
	}
	if got := Explain(town, t0.Add(2*time.Hour), cause); got != cause {
		t.Errorf("lapsed: Explain = %v, want cause unchanged", got)
	}
	if got := Explain(town, t0, nil); got != nil {
		t.Errorf("nil err: Explain = %v", got)
	}
}

func TestStale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		m     Marker
		stale bool
	}{
		{"fresh", Marker{Since: t0, Until: t0.Add(time.Hour)}, false},
		{"lapsed", Marker{Since: t0.Add(-2 * time.Hour), Until: t0.Add(-time.Hour)}, true},
		{"old", Marker{Since: t0.Add(-25 * time.Hour), Until: t0.Add(time.Hour)}, true},
		{"far until", Marker{Since: t0, Until: t0.Add(48 * time.Hour)}, true},
	}
	for _, c := range cases {
		if got := c.m.Stale(t0) != ""; got != c.stale {
			t.Errorf("%s: Stale = %q, want stale=%v", c.name, c.m.Stale(t0), c.stale)
		}
	}
}
