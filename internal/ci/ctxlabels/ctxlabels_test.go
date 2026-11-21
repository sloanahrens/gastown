package ctxlabels_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/ctxlabels"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	polecatseed "github.com/steveyegge/gastown/internal/polecat/seed"
)

const (
	// green is the commit the job builds at.
	green = "0123456789abcdef0123456789abcdef01234567"
	// other is a commit the context must not be staged at.
	other = "fedcba9876543210fedcba9876543210fedcba98"
	// bdPin is the bd commit the polecat image is built from.
	bdPin = "c1efb992c9ee42046f9a6188625b11fe88e05f53"
	// floor is the Go floor the image build takes.
	floor = "1.10.0"
)

// listing is what the polecat context's build prints for a context at the
// green commit.
func listing() string {
	return "note: devops is 3 commits behind\n" +
		"context: /runner/work/polecat-context.abc\n" +
		"label: org.gastown.gt-commit=" + green + "\n" +
		"label: org.gastown.bd-commit=" + bdPin + "\n" +
		"label: go-floor=" + floor + "\n"
}

// TestParseReadsTheLabelsTheBuildPrints holds the passing branch: the listing
// of a context at the green commit gives the bd commit and the Go floor.
func TestParseReadsTheLabelsTheBuildPrints(t *testing.T) {
	t.Parallel()
	got, err := ctxlabels.Parse(listing(), green)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.BDCommit != bdPin || got.GoFloor != floor {
		t.Errorf("Parse answered %+v, want %s and %s", got, bdPin, floor)
	}
}

// TestParseRefusesAContextOffTheGreenCommit holds the one-commit rule: a
// listing that names another commit, or none, is refused.
func TestParseRefusesAContextOffTheGreenCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		listing string
		want    string
	}{
		{
			name:    "another commit",
			listing: strings.Replace(listing(), "org.gastown.gt-commit="+green, "org.gastown.gt-commit="+other, 1),
			want:    other,
		},
		{
			name:    "no commit label",
			listing: strings.Replace(listing(), "label: org.gastown.gt-commit="+green+"\n", "", 1),
			want:    "names no org.gastown.gt-commit label",
		},
		{
			name:    "the commit label twice",
			listing: listing() + "label: org.gastown.gt-commit=" + green + "\n",
			want:    "names org.gastown.gt-commit twice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ctxlabels.Parse(tc.listing, green)
			if err == nil {
				t.Fatalf("Parse accepted a listing with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseRefusesAnUnusableBDOrGoFloor holds the label rule: the image build
// takes the bd commit and the Go floor, so a listing with neither, or with a
// value the build cannot take, is refused.
func TestParseRefusesAnUnusableBDOrGoFloor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		listing string
		want    string
	}{
		{
			name:    "no bd commit",
			listing: strings.Replace(listing(), "label: org.gastown.bd-commit="+bdPin+"\n", "", 1),
			want:    "names no org.gastown.bd-commit label",
		},
		{
			name:    "an empty bd commit",
			listing: strings.Replace(listing(), "org.gastown.bd-commit="+bdPin, "org.gastown.bd-commit=", 1),
			want:    "org.gastown.bd-commit label is \"\"",
		},
		{
			name:    "no Go floor",
			listing: strings.Replace(listing(), "label: go-floor="+floor+"\n", "", 1),
			want:    "names no go-floor label",
		},
		{
			name:    "a Go floor with a space",
			listing: strings.Replace(listing(), "go-floor="+floor, "go-floor=1.10 0", 1),
			want:    "go-floor label is \"1.10 0\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ctxlabels.Parse(tc.listing, green)
			if err == nil {
				t.Fatalf("Parse accepted a listing with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseRefusesALineThatIsNotAKeyValue holds the shape rule: a label line
// without "=" is a listing this parser does not understand, and it is not
// read as a label that is absent.
func TestParseRefusesALineThatIsNotAKeyValue(t *testing.T) {
	t.Parallel()
	_, err := ctxlabels.Parse(listing()+"label: go-floor\n", green)
	if err == nil {
		t.Fatal("Parse accepted a label line without a value")
	}
	if !strings.Contains(err.Error(), "is not a key=value label") {
		t.Errorf("Parse error = %q, want it to name the line's shape", err)
	}
}

// TestEnvWritesTheTwoBuildValues holds the output rule: the two values reach
// the environment file the image step reads.
func TestEnvWritesTheTwoBuildValues(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	got := ctxlabels.Labels{BDCommit: bdPin, GoFloor: floor}
	if err := got.Env(&out); err != nil {
		t.Fatalf("Env: %v", err)
	}
	want := "BD_COMMIT=" + bdPin + "\nGO_FLOOR=" + floor + "\n"
	if out.String() != want {
		t.Errorf("Env wrote %q, want %q", out.String(), want)
	}
}

// failingWriter is an environment output that cannot be written.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("no space left") }

// TestEnvRefusesAnEnvironmentItCannotWrite holds the write rule: a value the
// later step cannot read is a failure, not a pass.
func TestEnvRefusesAnEnvironmentItCannotWrite(t *testing.T) {
	t.Parallel()
	got := ctxlabels.Labels{BDCommit: bdPin, GoFloor: floor}
	err := got.Env(failingWriter{})
	if err == nil {
		t.Fatal("Env passed when the environment could not be written")
	}
	if !strings.Contains(err.Error(), "writing BD_COMMIT and GO_FLOOR") {
		t.Errorf("Env error = %q, want it to name the environment write", err)
	}
}

// bdPinMatches is the check TestBDPinMatchesImagePins runs on the image-pins
// bytes: the file must parse (daemonimage.ParsePins requires a line for each of
// bd, om, hm and devops, so a file without a bd line fails there) and its bd
// line must name want. Each failure names the file and, for a mismatch, both
// commits.
func bdPinMatches(imagePins []byte, want string) error {
	pins, err := daemonimage.ParsePins(imagePins)
	if err != nil {
		return fmt.Errorf("%s: %w", daemonimage.PinsPath, err)
	}
	var bd string
	for _, p := range pins {
		if p.Name == "bd" {
			bd = p.Commit
		}
	}
	if bd != want {
		return fmt.Errorf("bdPin = %s, but %s names bd at %s", want, daemonimage.PinsPath, bd)
	}
	return nil
}

// TestBDPinMatchesImagePins holds bdPin to the bd commit deploy/daemon/image-pins
// names, so a bump of the image pin cannot leave the polecat image's bd commit
// behind.
func TestBDPinMatchesImagePins(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(daemonimage.PinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", daemonimage.PinsPath, err)
	}
	text, err := config.ExpandForgeOwner(string(body), config.DefaultForgeOwner)
	if err != nil {
		t.Fatalf("%s: %v", daemonimage.PinsPath, err)
	}
	if err := bdPinMatches([]byte(text), bdPin); err != nil {
		t.Error(err)
	}
}

// TestBDPinMatchesRefuses drives each refusal of the drift check with a
// fixture, so the failing assertion is proven to fire and the real-file run
// above cannot pass for the wrong reason.
func TestBDPinMatchesRefuses(t *testing.T) {
	t.Parallel()
	const goodBD = "0123456789abcdef0123456789abcdef01234567"
	const otherBD = "fedcba9876543210fedcba9876543210fedcba98"
	const others = "om https://example.invalid/om.git " + goodBD + "\nhm https://example.invalid/hm.git " + goodBD + "\ndevops https://example.invalid/devops.git " + goodBD + "\n"
	const image = "bd https://example.invalid/bd.git " + goodBD + "\n" + others
	for _, tc := range []struct {
		name    string
		image   string
		want    string
		wantErr string
	}{
		{name: "the bd line names bdPin", image: image, want: goodBD},
		{name: "the bd line names another commit", image: image, want: otherBD,
			wantErr: "bdPin = " + otherBD + ", but " + daemonimage.PinsPath + " names bd at " + goodBD},
		{name: "the file has no bd line", image: others, want: goodBD,
			wantErr: daemonimage.PinsPath + ": missing pin for bd"},
		{name: "the file does not parse", image: "bd " + goodBD + "\n", want: goodBD,
			wantErr: daemonimage.PinsPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := bdPinMatches([]byte(tc.image), tc.want)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("bdPinMatches: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("bdPinMatches accepted the file")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("bdPinMatches error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// seedPinsPath is the polecat seed-pins file, relative to the repository root.
const seedPinsPath = "deploy/polecat/seed-pins"

// seedPinMatchesBDPin is the check TestSeedPinMatchesBDPin runs on the
// seed-pins bytes: the file must parse, name the beads rig, and pin it at want.
// The seed and the image's bd are the same repository, so one commit is both
// the bd binary's and the seeded beads source's. Each failure names the file.
func seedPinMatchesBDPin(seed []byte, want string) error {
	sources, err := polecatseed.Pins(seed)
	if err != nil {
		return fmt.Errorf("%s: %w", seedPinsPath, err)
	}
	for _, s := range sources {
		if s.Rig != "beads" {
			continue
		}
		if s.Commit != want {
			return fmt.Errorf("%s pins beads at %s, but bdPin is %s: bump both to the same commit", seedPinsPath, s.Commit, want)
		}
		return nil
	}
	return fmt.Errorf("%s names no beads line, but bdPin is %s", seedPinsPath, want)
}

// TestSeedPinMatchesBDPin holds the shipped seed-pins file's beads row to
// bdPin, so a bump of the bd pin cannot leave the seeded beads source at a
// different commit than the bd binary built from that source.
func TestSeedPinMatchesBDPin(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(seedPinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", seedPinsPath, err)
	}
	if err := seedPinMatchesBDPin(body, bdPin); err != nil {
		t.Error(err)
	}
}

// TestPreRollGuardPinsNameTheSamplesBuild pins the three files that name the
// bd commit to the beads build with bd sample and bd mol bond --root-only, so
// the pre-roll guard (gt-tgje6t) cannot be undone by a pin bump back to the
// schema-66 build (gt-fenu9p).
func TestPreRollGuardPinsNameTheSamplesBuild(t *testing.T) {
	t.Parallel()
	const samplesBuild = "c1efb992c9ee42046f9a6188625b11fe88e05f53"
	if bdPin != samplesBuild {
		t.Errorf("bdPin = %s, want %s", bdPin, samplesBuild)
	}
	image, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(daemonimage.PinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", daemonimage.PinsPath, err)
	}
	text, err := config.ExpandForgeOwner(string(image), config.DefaultForgeOwner)
	if err != nil {
		t.Fatalf("%s: %v", daemonimage.PinsPath, err)
	}
	if err := bdPinMatches([]byte(text), samplesBuild); err != nil {
		t.Error(err)
	}
	seed, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(seedPinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", seedPinsPath, err)
	}
	if err := seedPinMatchesBDPin(seed, samplesBuild); err != nil {
		t.Error(err)
	}
}

// TestSeedPinMatchesRefuses drives each refusal of the seed-pin check with a
// fixture, so the assertion above cannot pass for the wrong reason: a beads
// commit bdPin does not name, a file with no beads line, and one that does not
// parse. Each refusal names the seed-pins file.
func TestSeedPinMatchesRefuses(t *testing.T) {
	t.Parallel()
	const goodBD = "0123456789abcdef0123456789abcdef01234567"
	const otherBD = "fedcba9876543210fedcba9876543210fedcba98"
	seedWith := func(beads, devops string) string {
		return "# a seed\nbeads " + beads + "\ndevops " + devops + "\n"
	}
	for _, tc := range []struct {
		name    string
		seed    string
		want    string
		wantErr string
	}{
		{name: "the beads line names bdPin", seed: seedWith(goodBD, otherBD), want: goodBD},
		{name: "the beads line names another commit", seed: seedWith(otherBD, goodBD), want: goodBD,
			wantErr: seedPinsPath + " pins beads at " + otherBD + ", but bdPin is " + goodBD},
		{name: "the file has no beads line", seed: "devops " + goodBD + "\n", want: goodBD,
			wantErr: seedPinsPath + " names no beads line, but bdPin is " + goodBD},
		{name: "the file does not parse", seed: "beads\n", want: goodBD,
			wantErr: seedPinsPath + ": line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := seedPinMatchesBDPin([]byte(tc.seed), tc.want)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("seedPinMatchesBDPin: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("seedPinMatchesBDPin accepted the file")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("seedPinMatchesBDPin error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// knownOlderBDBuilds lists the bd commits deploy/daemon/image-pins named before
// the samples build (gt-4zyqzl). Pin locations are from `git log -S<sha> --
// deploy/daemon/image-pins` on origin/main; each entry is an ancestor of
// daemonimage.BDSamplesFloor (0f9f640f) and of the current bd pin (c1efb992),
// per `git merge-base --is-ancestor` in the beads repository (all exit 0).
var knownOlderBDBuilds = []string{
	// b7e5e9ce: pinned in 583fb26ff (gt-nmy5yu), 5c3305180 (gt-smitrf), and
	// 914b0586e (gt-fenu9p, a revert to the schema-66 build); left by 1ba150e37 (gt-tgje6t).
	"b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
	// 44cb296b: pinned in bbb4bf198 (gt-xvaxs9); left by 583fb26ff.
	"44cb296bbe4180f31553cb61f753aa8bfb3f9321",
	// ecf0fc58: pinned in 6d36955f0 (gt-f4bcht); left by bbb4bf198.
	"ecf0fc58c789b6d5a158bcf4b485662bc15f296d",
	// d0f0010e: pinned in 343d2c5a4 (gt-gg9hv.31); left by 6d36955f0.
	"d0f0010ef037e8de0006f2a4d0e01019120a5d8c",
}

// TestKnownOlderBDBuildsAreFullShas holds the list to 40 lowercase hex commits
// with no repeats, so the image-pins read is never handed a bad or duplicate sha.
// fullShaProblems names each way a list of commit ids is not a list of distinct
// full lowercase shas: a wrong length, a character outside 0-9a-f, a repeat.
func fullShaProblems(entries []string) []string {
	var problems []string
	seen := make(map[string]bool, len(entries))
	for _, sha := range entries {
		if len(sha) != 40 {
			problems = append(problems, fmt.Sprintf("%q has %d characters, want 40", sha, len(sha)))
		}
		for _, r := range sha {
			if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
				problems = append(problems, fmt.Sprintf("%q holds %q, want only lowercase hex", sha, r))
				break
			}
		}
		if seen[sha] {
			problems = append(problems, fmt.Sprintf("%q is named twice", sha))
		}
		seen[sha] = true
	}
	return problems
}

func TestKnownOlderBDBuildsAreFullShas(t *testing.T) {
	t.Parallel()
	for _, p := range fullShaProblems(knownOlderBDBuilds) {
		t.Errorf("knownOlderBDBuilds entry %s", p)
	}
}

// TestFullShaProblemsNamesEachFailure drives every branch of the entry check:
// the real list passes, so only a case that breaks the rule shows the check can fail.
func TestFullShaProblemsNamesEachFailure(t *testing.T) {
	t.Parallel()
	good := "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f"
	for _, tc := range []struct {
		name    string
		entries []string
		want    []string
	}{
		{"a full lowercase sha passes", []string{good}, nil},
		{"a 39 character sha is short", []string{good[:39]}, []string{"has 39 characters, want 40"}},
		{"a 41 character sha is long", []string{good + "0"}, []string{"has 41 characters, want 40"}},
		{"an uppercase sha is refused", []string{strings.ToUpper(good)}, []string{"want only lowercase hex"}},
		{"a non-hex character is refused", []string{good[:39] + "g"}, []string{"want only lowercase hex"}},
		{"a repeated sha is refused", []string{good, good}, []string{"is named twice"}},
		{"an empty entry is short", []string{""}, []string{"has 0 characters, want 40"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fullShaProblems(tc.entries)
			if len(got) != len(tc.want) {
				t.Fatalf("fullShaProblems(%q) = %q, want %d problem(s)", tc.entries, got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// realPinsText is the pins file as the gate reads it, with its forge owner
// expanded. It is the one reader of the real file: pinsWithBD rewrites its
// output and the test compares that to a parse of this text, so the other pins
// are held to the real file and not to the helper's own result.
func realPinsText(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(daemonimage.PinsPath)))
	if err != nil {
		t.Fatalf("reading %s: %v", daemonimage.PinsPath, err)
	}
	text, err := config.ExpandForgeOwner(string(body), config.DefaultForgeOwner)
	if err != nil {
		t.Fatalf("%s: %v", daemonimage.PinsPath, err)
	}
	return text
}

// pinsWithBD returns the image-pins text with its bd line naming commit. It
// fails the test when the file has no bd line.
func pinsWithBD(t *testing.T, commit string) []byte {
	t.Helper()
	text := realPinsText(t)
	var out []string
	var named bool
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "bd" {
			line, named = "bd "+f[1]+" "+commit, true
		}
		out = append(out, line)
	}
	if !named {
		t.Fatalf("%s names no bd line", daemonimage.PinsPath)
	}
	return []byte(strings.Join(out, "\n"))
}

// TestPinsWithBDNamesTheCommit holds the fixture to its contract: its bd pin is
// the commit given, and its other pins are the real file's.
func TestPinsWithBDNamesTheCommit(t *testing.T) {
	t.Parallel()
	base, err := daemonimage.ParsePins([]byte(realPinsText(t)))
	if err != nil {
		t.Fatalf("ParsePins of the real file: %v", err)
	}
	for _, commit := range []string{bdPin, "fedcba9876543210fedcba9876543210fedcba98"} {
		pins, err := daemonimage.ParsePins(pinsWithBD(t, commit))
		if err != nil {
			t.Fatalf("ParsePins: %v", err)
		}
		if len(pins) != len(base) {
			t.Fatalf("pinsWithBD(%s) gave %d pins, want %d", commit, len(pins), len(base))
		}
		for i, p := range pins {
			want := base[i]
			if want.Name == "bd" {
				want.Commit = commit
			}
			if p != want {
				t.Errorf("pin %s = %+v, want %+v", p.Name, p, want)
			}
		}
	}
}
