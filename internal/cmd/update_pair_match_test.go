package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// pairMatchCommit is the commit the derived pair was built from.
const pairMatchCommit = "c0ffee0c0ffee0c0ffee0c0ffee0c0ffee0c0ffe"

// pairMatchRef is a digest reference for image whose digest starts with prefix,
// so each row names its digests by their first 12 hex.
func pairMatchRef(image, prefix string) string {
	return "registry.internal:5000/town/" + image + "@sha256:" + prefix + strings.Repeat("0", 52)
}

// pairMatchPair is the derived pair for pairMatchCommit; each row edits a fresh copy.
func pairMatchPair() imagevalues.Values {
	return imagevalues.Values{
		Daemon:  pairMatchRef(imagevalues.DaemonImage, "dddddddddddd"),
		Polecat: pairMatchRef(imagevalues.PolecatImage, "eeeeeeeeeeee"),
		Bd:      pairMatchRef(imagevalues.BdImage, "bbbbbbbbbbbb"),
		Commit:  pairMatchCommit,
	}
}

// TestPairMatchesDerived holds the file's pair to the derived pair: equal images
// pass, and a differing daemon, polecat or bd digest is refused naming the field
// and the first 12 hex of both digests, the first differing field first. A
// derived pair with no full commit, and an image that is not a digest reference,
// are refused: a pair nothing could compare is not a match.
func TestPairMatchesDerived(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// edit changes the file's pair (f) or the derived pair (d); nil leaves both as derived.
		edit func(f, d *imagevalues.Values)
		// want is empty when the pair matches; otherwise every string must appear in the refusal.
		want []string
		// omits are strings the refusal must not carry.
		omits []string
	}{
		{name: "equal pairs with a bd image pass"},
		{name: "a file naming no bd image passes against a derived pair that has one", edit: func(f, _ *imagevalues.Values) { f.Bd = "" }},
		{name: "a daemon digest that differs names daemon and both prefixes", edit: func(f, _ *imagevalues.Values) { f.Daemon = pairMatchRef(imagevalues.DaemonImage, "111111111111") }, want: []string{"daemon", "sha256:111111111111", "sha256:dddddddddddd"}, omits: []string{"111111111111" + strings.Repeat("0", 52)}},
		{name: "a polecat digest that differs names polecat and both prefixes", edit: func(f, _ *imagevalues.Values) { f.Polecat = pairMatchRef(imagevalues.PolecatImage, "222222222222") }, want: []string{"polecat", "sha256:222222222222", "sha256:eeeeeeeeeeee"}},
		{name: "a bd digest that differs names bd and both prefixes", edit: func(f, _ *imagevalues.Values) { f.Bd = pairMatchRef(imagevalues.BdImage, "333333333333") }, want: []string{"bd", "sha256:333333333333", "sha256:bbbbbbbbbbbb"}},
		{
			name: "when daemon and polecat both differ the daemon is named",
			edit: func(f, _ *imagevalues.Values) {
				f.Daemon = pairMatchRef(imagevalues.DaemonImage, "111111111111")
				f.Polecat = pairMatchRef(imagevalues.PolecatImage, "222222222222")
			},
			want:  []string{"daemon", "sha256:111111111111"},
			omits: []string{"polecat"},
		},
		{name: "the same digest under another repository is refused", edit: func(f, _ *imagevalues.Values) {
			f.Daemon = "registry.internal:5000/rogue/gastown-daemon@sha256:dddddddddddd" + strings.Repeat("0", 52)
		}, want: []string{"daemon", "different repositories"}},
		{name: "a derived pair with no commit is refused", edit: func(_, d *imagevalues.Values) { d.Commit = "" }, want: []string{"no full commit"}},
		{name: "a file daemon that is a tag is refused", edit: func(f, _ *imagevalues.Values) { f.Daemon = "registry.internal:5000/town/gastown-daemon:latest" }, want: []string{"file's daemon image", "not a digest reference"}},
		{name: "a file with no polecat image is refused", edit: func(f, _ *imagevalues.Values) { f.Polecat = "" }, want: []string{"file's polecat image", "not a digest reference"}},
		{name: "a derived daemon that is a tag is refused", edit: func(_, d *imagevalues.Values) { d.Daemon = "registry.internal:5000/town/gastown-daemon:latest" }, want: []string{"derived daemon image", "not a digest reference"}},
		{name: "a derived bd that is a tag is refused", edit: func(_, d *imagevalues.Values) { d.Bd = "registry.internal:5000/town/gastown-bd:latest" }, want: []string{"derived bd image", "not a digest reference"}},
		{name: "a derived polecat that is a tag is refused", edit: func(_, d *imagevalues.Values) { d.Polecat = "registry.internal:5000/town/gastown-polecat:latest" }, want: []string{"derived polecat image", "not a digest reference"}},
		{name: "a file bd that is a tag is refused", edit: func(f, _ *imagevalues.Values) { f.Bd = "registry.internal:5000/town/gastown-bd:latest" }, want: []string{"file's bd image", "not a digest reference"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := pairMatchPair(), pairMatchPair()
			if tc.edit != nil {
				tc.edit(&f, &d)
			}
			err := pairMatchesDerived(f, d)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("pairMatchesDerived refused a matching pair: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("pairMatchesDerived accepted a pair it should refuse; want a refusal naming %q", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err, w)
				}
			}
			for _, o := range tc.omits {
				if strings.Contains(err.Error(), o) {
					t.Errorf("refusal %q contains %q, which it must not", err, o)
				}
			}
		})
	}
}

// TestPairMatchesDerivedMissing refuses a file image the derived pair does not
// have: a bd image the file names and the derivation left out, or a daemon or
// polecat image the derivation left empty.
func TestPairMatchesDerivedMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(d *imagevalues.Values)
		want string
	}{
		{name: "a bd image the file names and the derivation lacks is refused", edit: func(d *imagevalues.Values) { d.Bd = "" }, want: "derived pair has no bd image"},
		{name: "a derived daemon image left empty is refused", edit: func(d *imagevalues.Values) { d.Daemon = "" }, want: "derived pair has no daemon image"},
		{name: "a derived polecat image left empty is refused", edit: func(d *imagevalues.Values) { d.Polecat = "" }, want: "derived pair has no polecat image"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := pairMatchPair(), pairMatchPair()
			tc.edit(&d)
			err := pairMatchesDerived(f, d)
			if err == nil {
				t.Fatalf("pairMatchesDerived accepted a file image the derivation lacks; want a refusal containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not contain %q", err, tc.want)
			}
		})
	}
}
