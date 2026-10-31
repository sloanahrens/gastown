package buildflags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDarwinCgoEnv: the cgo entries the build needs are added when the caller
// set neither flag and a keg holds the header, and are not when it set one or
// no keg has the header.
func TestDarwinCgoEnv(t *testing.T) {
	t.Parallel()
	const keg = "/opt/homebrew/opt/icu4c@78"
	wantEntries := []string{"CGO_CPPFLAGS=-I" + keg + "/include", "CGO_LDFLAGS=-L" + keg + "/lib"}
	cases := []struct {
		name   string
		prefix string
		base   []string
		want   []string
	}{
		{
			name: "no cgo flag set", prefix: keg,
			base: []string{"PATH=/bin"},
			want: wantEntries,
		},
		{
			name: "CGO_CPPFLAGS already set", prefix: keg,
			base: []string{"CGO_CPPFLAGS=-I/somewhere/include"},
			want: nil,
		},
		{
			name: "CGO_LDFLAGS already set", prefix: keg,
			base: []string{"CGO_LDFLAGS=-L/somewhere/lib"},
			want: nil,
		},
		{
			name: "an empty CGO_CPPFLAGS counts as unset", prefix: keg,
			base: []string{"CGO_CPPFLAGS="},
			want: wantEntries,
		},
		{
			name: "no icu4c keg", prefix: "",
			base: []string{"PATH=/bin"},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := darwinCgoEnv(tc.base, tc.prefix)
			if len(got) != len(tc.want) {
				t.Fatalf("darwinCgoEnv = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("darwinCgoEnv = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestIntegrationHelperTakesItsFlagsFromHere pins the single source: the
// internal/cmd helper builds with buildflags.LDFlags rather than a second copy
// of the literal, so the sweep and the helper cannot drift apart (gt-2daks).
func TestIntegrationHelperTakesItsFlagsFromHere(t *testing.T) {
	t.Parallel()
	const helper = "../cmd/test_helpers_integration_test.go"
	src, err := os.ReadFile(filepath.Join(helper))
	if err != nil {
		t.Fatalf("read %s: %v", helper, err)
	}
	text := string(src)
	if !strings.Contains(text, "buildflags.LDFlags") {
		t.Errorf("%s does not build with buildflags.LDFlags: the helper and the sweep can carry different ldflags", helper)
	}
	if strings.Contains(text, LDFlags) {
		t.Errorf("%s still carries the ldflags literal: it should name buildflags.LDFlags", helper)
	}
}
