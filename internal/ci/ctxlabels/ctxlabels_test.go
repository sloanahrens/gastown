package ctxlabels_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/ctxlabels"
)

const (
	// green is the commit the job builds at.
	green = "0123456789abcdef0123456789abcdef01234567"
	// other is a commit the context must not be staged at.
	other = "fedcba9876543210fedcba9876543210fedcba98"
	// bdPin is the bd commit the polecat image is built from.
	bdPin = "d0f0010ef037e8de0006f2a4d0e01019120a5d8c"
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
