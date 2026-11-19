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
	bdPin = "0f9f640fe0f1a4162f3aebdef7994135b1167a85"
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
	const samplesBuild = "0f9f640fe0f1a4162f3aebdef7994135b1167a85"
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
