package dashboard

import (
	"strings"
	"testing"
)

// The header's hold chip draws the reader's verdict and never its own: a hold
// in amber, a dispatching town in the quiet dim, and a file it could not read
// as "hold?" — never "dispatch on", which would clear a hold nothing had ruled
// out (gt-ifijm, gt-zzkhh).
func TestHoldChipDrawsTheReadersReading(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		hold  map[string]any
		text  string
		class string
		title []string
	}{
		{
			name:  "the hold is on",
			hold:  map[string]any{"present": true, "note": "sloan: paused for the night", "age_seconds": 7200},
			text:  "hold",
			class: "warn",
			title: []string{"set 2h00m ago", "sloan: paused for the night"},
		},
		{
			name:  "the town is dispatching",
			hold:  map[string]any{"present": false},
			text:  "dispatch on",
			class: "quiet",
			title: []string{"no operator dispatch hold"},
		},
		{
			name:  "the file could not be read",
			hold:  map[string]any{"present": nil, "error": "the hold path is not a regular file"},
			text:  "hold?",
			class: "warn",
			title: []string{"the hold path is not a regular file"},
		},
		{
			// Neither present nor absent, and no error text: the reading ruled
			// out nothing, so the chip says so rather than "dispatch on".
			name:  "the reading says neither present nor absent",
			hold:  map[string]any{"present": nil},
			text:  "hold?",
			class: "warn",
			title: []string{"unknown"},
		},
		{
			name:  "the reader has not reported",
			hold:  nil,
			text:  "dispatch ?",
			class: "quiet",
			title: []string{"not been read yet"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			state := map[string]any{}
			if tc.hold != nil {
				state["hold"] = tc.hold
			}
			dom.call(t, "renderHold", state)

			chip := dom.element(t, "hold")
			if chip.Hidden {
				t.Error("the chip is left hidden, so the operator reads no hold at all")
			}
			if chip.Text != tc.text {
				t.Errorf("the chip reads %q, want %q", chip.Text, tc.text)
			}
			if chip.Class != tc.class {
				t.Errorf("the chip wears %q, want %q", chip.Class, tc.class)
			}
			for _, want := range tc.title {
				if !strings.Contains(chip.Title, want) {
					t.Errorf("the chip's title reads %q, with no %q", chip.Title, want)
				}
			}
		})
	}
}

// The note is text the operator wrote in a file, so the chip carries it as
// text: markup in a note is printed, not parsed into the header.
func TestHoldChipCarriesTheNoteAsText(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderHold", map[string]any{
		"hold": map[string]any{"present": true, "note": `<b>ops</b> & "co"`, "age_seconds": 60},
	})

	chip := dom.element(t, "hold")
	if !strings.Contains(chip.Title, `<b>ops</b> & "co"`) {
		t.Errorf("the chip's title reads %q, not the note as written", chip.Title)
	}
	if chip.Kids != 0 {
		t.Errorf("the chip built %d children from the note, so the note was inserted as markup", chip.Kids)
	}
	if chip.Text != "hold" {
		t.Errorf("the chip reads %q, want the word alone", chip.Text)
	}
}

// The chip sits in the header right after the verdict pill, and the page's
// redraw draws it (gt-zzkhh).
func TestHoldChipSitsInTheHeaderAfterTheVerdictPill(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if n := strings.Count(page, `id="hold"`); n != 1 {
		t.Errorf("the page has %d hold chips, want 1", n)
	}
	header := page[strings.Index(page, "<header>"):strings.Index(page, "</header>")]
	verdict, chip := strings.Index(header, `id="verdict"`), strings.Index(header, `id="hold"`)
	if verdict < 0 || chip < 0 {
		t.Fatal("the header carries no verdict pill or no hold chip")
	}
	if chip < verdict {
		t.Error("the hold chip sits before the verdict pill it belongs after")
	}
	if !strings.Contains(pageFunc(t, "renderAll"), "renderHold(state)") {
		t.Error("the page's redraw does not draw the hold chip, so it never moves")
	}
}

// Each class the drawing puts on the chip is one the stylesheet colours for the
// chip: the class is the one the drawn chip wears, and the colour is read from
// the rule's declarations, not matched as text, so a reformatted rule still
// counts and a class the stylesheet never colours does not (gt-zzkhh).
func TestHoldChipClassesAreColouredByTheStylesheet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hold map[string]any
		want string
	}{
		{"the hold is on", map[string]any{"present": true}, "var(--amber)"},
		{"the town is dispatching", map[string]any{"present": false}, "var(--dim)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderHold", map[string]any{"hold": tc.hold})
			chip := dom.element(t, "hold")
			if got := holdChipColor(t, chip.Class); got != tc.want {
				t.Errorf("the chip wears %q, which the stylesheet colours %q, want %q", chip.Class, got, tc.want)
			}
		})
	}
}

// holdChipColor is the colour the stylesheet sets on the chip when it wears
// class: the declarations of the rule whose subject is the chip itself.
func holdChipColor(t *testing.T, class string) string {
	t.Helper()

	for _, rule := range strings.Split(pageStyle(t), "}") {
		sel, body, ok := strings.Cut(rule, "{")
		if !ok || !strings.Contains(sel, "#hold") || !selectsClass(sel, class) {
			continue
		}
		for _, decl := range strings.Split(body, ";") {
			prop, val, ok := strings.Cut(decl, ":")
			if ok && strings.TrimSpace(prop) == "color" {
				return strings.TrimSpace(val)
			}
		}
	}
	return ""
}
