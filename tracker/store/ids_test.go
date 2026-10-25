package store

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestRandomBase36MapsBytesUniformlyAndRejectsTheBias(t *testing.T) {
	t.Parallel() // unit tier: pure over a fixed byte source
	// Bytes 252..255 would make 0..3 likelier (256 is not a multiple of 36),
	// so they are skipped: 252 and 255 are dropped, 0 and 35 are kept.
	got, err := randomBase36(bytes.NewReader([]byte{252, 0, 255, 35, 10}), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0za" {
		t.Errorf("randomBase36 = %q, want 0za (0, 35 and 10 in base 36)", got)
	}
}

func TestRandomBase36FailsOnAShortSource(t *testing.T) {
	t.Parallel()
	if got, err := randomBase36(bytes.NewReader([]byte{1, 2}), 5); err == nil {
		t.Errorf("randomBase36 with too few bytes = %q, nil; want an error, not a weaker id", got)
	}
}

func TestIDShapes(t *testing.T) {
	t.Parallel()
	src := func() *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte{7, 200, 13, 99}, 16)) }
	id, err := newID(src(), "gt")
	if err != nil || !regexp.MustCompile(`^gt-[0-9a-z]{5}$`).MatchString(id) {
		t.Errorf("newID = %q, %v; want gt- and five base-36 characters", id, err)
	}
	w, err := newWispID(src(), "hq")
	if err != nil || !regexp.MustCompile(`^hq-wisp-[0-9a-z]{6}$`).MatchString(w) {
		t.Errorf("newWispID = %q, %v; want hq-wisp- and six base-36 characters", w, err)
	}
}

func TestRigOfID(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{
		"gt-abc12": "gt", "hq-wisp-abc123": "hq", "gt-22hdp.10": "gt", "gt": "", "-x": "", "": "",
	} {
		if got := rigOfID(id); got != want {
			t.Errorf("rigOfID(%q) = %q, want %q", id, got, want)
		}
	}
	if !strings.HasPrefix("gt-abc12", rigOfID("gt-abc12")+"-") {
		t.Error("an id must start with its rig and a dash")
	}
}
