package cmd

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// testBdBaseSHA is the commit the beads base image was tagged with; its
	// first twelve characters are the tag the fake registry is asked for.
	testBdBaseSHA = "0123456789abcdef0123456789abcdef01234567"
	// testBdBaseTag is testBdBaseSHA's tag.
	testBdBaseTag = "0123456789ab"
	// testBdBaseUser and testBdBaseToken are the viewer credential every test
	// wires; the token must never reach the command's output or an error.
	testBdBaseUser  = "bot-viewer"
	testBdBaseToken = "viewer-token-7c31e0"
	// testBdBasePin is the pin file path every test that passes --pin names.
	testBdBasePin = "pin/base-pin"
)

// testBdBaseDigest is a well-formed digest the registry answers for the tag.
var testBdBaseDigest = "sha256:" + strings.Repeat("ab", 32)

// testBdBaseOldDigest is a well-formed digest the pin holds before a run that
// moves it.
var testBdBaseOldDigest = "sha256:" + strings.Repeat("cd", 32)

// testBdBaseRegistry is the registry a fake town publishes its images to.
var testBdBaseRegistry = imagevalues.Registry{Scheme: "https", Host: "forgejo:3000", Owner: "bot-registry"}

// fakeBdBase drives updateBdBaseSeams: the registry's answer for the tag, the
// pin file's contents, and the record of every read and write the command makes.
type fakeBdBase struct {
	digest   string
	found    bool
	readErr  error
	regErr   error
	credErr  error
	writeErr error

	// pin is the pin file's bytes; pinMissing makes ReadPin report not-exist.
	pin        []byte
	pinMissing bool
	pinReadErr error

	tagReads  int
	tagImage  string
	tagTag    string
	pinReads  []string
	pinWrites []string
	written   []byte
}

// seams returns the fake as the command's seams.
func (f *fakeBdBase) seams() updateBdBaseSeams {
	return updateBdBaseSeams{
		FindTownRoot: func() (string, error) { return "/town", nil },
		Registry: func(string) (imagevalues.Registry, error) {
			if f.regErr != nil {
				return imagevalues.Registry{}, f.regErr
			}
			return testBdBaseRegistry, nil
		},
		Credentials: func() (string, string, error) {
			if f.credErr != nil {
				return "", "", f.credErr
			}
			return testBdBaseUser, testBdBaseToken, nil
		},
		TagDigest: func(_ context.Context, _ imagevalues.Registry, _, _, image, tag string) (string, bool, error) {
			f.tagReads++
			f.tagImage, f.tagTag = image, tag
			return f.digest, f.found, f.readErr
		},
		ReadPin: func(path string) ([]byte, error) {
			f.pinReads = append(f.pinReads, path)
			if f.pinReadErr != nil {
				return nil, f.pinReadErr
			}
			if f.pinMissing {
				return nil, fs.ErrNotExist
			}
			return f.pin, nil
		},
		WritePin: func(path string, data []byte) error {
			f.pinWrites = append(f.pinWrites, path)
			if f.writeErr != nil {
				return f.writeErr
			}
			f.written = data
			f.pin = data
			return nil
		},
	}
}

// runBdBase runs `gt update bd-base` over the fake with args and returns its
// output and error.
func runBdBase(t *testing.T, f *fakeBdBase, args ...string) (string, error) {
	t.Helper()
	cmd := newUpdateBdBaseCmd(f.seams())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestUpdateBdBaseWritesDigest: a tag the registry holds is resolved to its
// digest, the pin file gets the digest reference, and the digest is printed.
func TestUpdateBdBaseWritesDigest(t *testing.T) {
	t.Parallel()
	f := &fakeBdBase{digest: testBdBaseDigest, found: true, pinMissing: true}

	out, err := runBdBase(t, f, "--sha", testBdBaseSHA, "--pin", testBdBasePin)
	if err != nil {
		t.Fatalf("gt update bd-base: %v", err)
	}
	if f.tagImage != "beads-bd" || f.tagTag != testBdBaseTag {
		t.Errorf("registry read %s:%s, want beads-bd:%s", f.tagImage, f.tagTag, testBdBaseTag)
	}
	want := "forgejo:3000/bot-registry/beads-bd@" + testBdBaseDigest + "\n"
	if string(f.written) != want {
		t.Errorf("pin written %q, want %q", f.written, want)
	}
	if !strings.Contains(out, testBdBaseDigest) {
		t.Errorf("output %q does not print the digest it wrote %s", out, testBdBaseDigest)
	}
}

// TestUpdateBdBaseWritesOnlyWhenDigestChanged: a second run for the same digest
// leaves the pin file untouched, and a digest the pin does not hold replaces it.
func TestUpdateBdBaseWritesOnlyWhenDigestChanged(t *testing.T) {
	t.Parallel()
	ref := func(d string) []byte { return []byte("forgejo:3000/bot-registry/beads-bd@" + d + "\n") }
	tests := []struct {
		name       string
		pin        []byte
		wantWrites int
	}{
		{name: "same digest leaves the file alone", pin: ref(testBdBaseDigest), wantWrites: 0},
		{name: "a moved digest replaces the file", pin: ref(testBdBaseOldDigest), wantWrites: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeBdBase{digest: testBdBaseDigest, found: true, pin: tc.pin}

			out, err := runBdBase(t, f, "--sha", testBdBaseSHA, "--pin", testBdBasePin)
			if err != nil {
				t.Fatalf("gt update bd-base: %v", err)
			}
			if len(f.pinWrites) != tc.wantWrites {
				t.Errorf("pin writes = %d, want %d", len(f.pinWrites), tc.wantWrites)
			}
			if string(f.pin) != string(ref(testBdBaseDigest)) {
				t.Errorf("pin holds %q, want %q", f.pin, ref(testBdBaseDigest))
			}
			if !strings.Contains(out, testBdBaseDigest) {
				t.Errorf("output %q does not print the digest %s", out, testBdBaseDigest)
			}
		})
	}
}

// TestUpdateBdBaseDefaultsPinUnderGastown: without --pin the pin file is
// deploy/bd/base-pin under the town's gastown directory.
func TestUpdateBdBaseDefaultsPinUnderGastown(t *testing.T) {
	t.Parallel()
	f := &fakeBdBase{digest: testBdBaseDigest, found: true, pinMissing: true}

	if _, err := runBdBase(t, f, "--sha", testBdBaseSHA); err != nil {
		t.Fatalf("gt update bd-base: %v", err)
	}
	want := filepath.Join("/town", "gastown", "deploy", "bd", "base-pin")
	if len(f.pinWrites) != 1 || f.pinWrites[0] != want {
		t.Errorf("pin written at %q, want %q", f.pinWrites, want)
	}
}

// TestUpdateBdBaseReportsAFailedPinWrite: a write the disk refuses is an error,
// not a success, and the pin the command was replacing is what the fake still
// holds.
func TestUpdateBdBaseReportsAFailedPinWrite(t *testing.T) {
	t.Parallel()
	current := []byte("forgejo:3000/bot-registry/beads-bd@" + testBdBaseOldDigest + "\n")
	f := &fakeBdBase{digest: testBdBaseDigest, found: true, pin: current, writeErr: errors.New("disk full")}

	out, err := runBdBase(t, f, "--sha", testBdBaseSHA, "--pin", testBdBasePin)
	if err == nil {
		t.Fatalf("gt update bd-base succeeded (output %q), want the write error", out)
	}
	if strings.Contains(out, testBdBaseDigest) {
		t.Errorf("output %q reports the digest as written after a failed write", out)
	}
	if string(f.pin) != string(current) {
		t.Errorf("pin changed to %q after a failed write, want %q", f.pin, current)
	}
}

// TestUpdateBdBaseRefuses: an answer the command cannot pin is refused, and the
// pin file is never written or replaced.
func TestUpdateBdBaseRefuses(t *testing.T) {
	t.Parallel()
	current := []byte("forgejo:3000/bot-registry/beads-bd@" + testBdBaseOldDigest + "\n")
	tests := []struct {
		name string
		f    fakeBdBase
		args []string
		// want is a phrase the refusal names, so each branch is told apart.
		want string
	}{
		{name: "registry has no such tag", f: fakeBdBase{found: false}, want: "has no tag"},
		{name: "registry answers no digest", f: fakeBdBase{found: true, digest: ""}, want: "not sha256"},
		{name: "registry answers a tag, not a digest", f: fakeBdBase{found: true, digest: "beads-bd:" + testBdBaseTag}},
		{name: "registry answers an uppercase digest", f: fakeBdBase{found: true, digest: "sha256:" + strings.Repeat("AB", 32)}},
		{name: "registry read fails", f: fakeBdBase{found: true, digest: testBdBaseDigest, readErr: errors.New("the registry answered HTTP 500")}},
		{name: "registry cannot be resolved", f: fakeBdBase{regErr: errors.New("no registry")}},
		{name: "viewer credential cannot be read", f: fakeBdBase{found: true, digest: testBdBaseDigest, credErr: errors.New("no token file")}},
		{name: "existing pin cannot be read", f: fakeBdBase{found: true, digest: testBdBaseDigest, pinReadErr: errors.New("permission denied")}},
		{name: "short commit", f: fakeBdBase{found: true, digest: testBdBaseDigest}, args: []string{"--sha", "0123456789ab"}},
		{name: "empty pin path", f: fakeBdBase{found: true, digest: testBdBaseDigest}, args: []string{"--sha", testBdBaseSHA, "--pin", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.f
			f.pin = current
			args := tc.args
			if args == nil {
				args = []string{"--sha", testBdBaseSHA, "--pin", testBdBasePin}
			}

			out, err := runBdBase(t, &f, args...)
			if err == nil {
				t.Fatalf("gt update bd-base succeeded (output %q), want a refusal", out)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
			if len(f.pinWrites) != 0 {
				t.Errorf("pin written %q on a refusal, want none", f.pinWrites)
			}
			if string(f.pin) != string(current) {
				t.Errorf("pin changed to %q on a refusal, want %q", f.pin, current)
			}
			if strings.Contains(err.Error(), testBdBaseToken) || strings.Contains(out, testBdBaseToken) {
				t.Errorf("the viewer token reached the output or error: %v", err)
			}
		})
	}
}
