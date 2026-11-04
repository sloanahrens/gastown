package templates

import (
	"strings"
	"testing"
)

// within answers "cannot tell" as inside, so the scratch root check refuses a
// pair it could not compare instead of accepting it (gt-qpeei.29).
func TestWithin_ACannotComparePairCountsAsInside(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, root string
		want             bool
	}{
		{"same path", "/towns/live", "/towns/live", true},
		{"under the root", "/towns/live/crew", "/towns/live", true},
		{"beside the root", "/towns/scratch", "/towns/live", false},
		{"above the root", "/towns", "/towns/live", false},
		{"same path in another case", "/Towns/Live", "/towns/live", true},
		{"relative path against an absolute root", ".", "/towns/live", true},
		{"absolute path against a relative root", "/towns/live", ".", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := within(c.path, c.root); got != c.want {
				t.Fatalf("within(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
			}
		})
	}
}

// A live spec takes the om token file: the refusal that kept the field off a
// live spec is gone, and the checks that remain are the absolute path and the
// control character, on the host path the live render mounts (gt-yaqct).
func TestComposeSpecValidateLiveOMToken(t *testing.T) {
	t.Parallel()
	liveSpec := func() ComposeSpec {
		return ComposeSpec{
			Project:       "gt-town",
			ContainerName: "gt-town-daemon-1",
			Image:         "gt-daemon:4f2a1c9",
			TownRoot:      "/towns/live",
			StopGrace:     MinimumStopGrace,
			DoltHost:      "host.docker.internal",
		}
	}
	withToken := liveSpec()
	withToken.OMTokenFile = "/ops/om/token"
	withToken.OMConfigFile = "/towns/live/.runtime/container/om-config.json"
	if err := withToken.validate(); err != nil {
		t.Fatalf("a live spec with an absolute token path = %v, want no error: the live render mounts it", err)
	}

	relative := liveSpec()
	relative.OMTokenFile = "secrets/om-token"
	control := liveSpec()
	control.OMTokenFile = "/ops/om\ntoken"
	for name, tc := range map[string]struct {
		spec ComposeSpec
		want string
	}{
		"a relative path": {relative, "is not absolute"},
		"a control char":  {control, "control character"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// checkScratchRoot refuses a relative root on either side by name, before any
// resolution: a relative scratch root (for example "." with the working directory
// at the live root) must never read as clear.
func TestCheckScratchRoot_RefusesARelativeRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		town, live   string
		wantContains string
	}{
		{"relative scratch root", ".", "/towns/live", "must both be absolute"},
		{"relative live root", "/towns/scratch", "towns/live", "must both be absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ComposeSpec{TownRoot: c.town, LiveTownRoot: c.live}.checkScratchRoot()
			if err == nil || !strings.Contains(err.Error(), c.wantContains) {
				t.Fatalf("checkScratchRoot(%q, %q) = %v, want an error containing %q", c.town, c.live, err, c.wantContains)
			}
		})
	}
}
