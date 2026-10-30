package scratchtown_test

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/scratchtown"
)

// testIdentity is the identity the fixtures are judged against: a fixed home
// the test owns, so nothing here reads a developer's home directory.
func testIdentity() scratchtown.Identity {
	return scratchtown.Identity{Parent: filepath.Join(string(filepath.Separator), "home", "op", scratchtown.ParentDir)}
}

// composeLabels is the label set the project's compose stamps on its
// containers.
func composeLabels(id scratchtown.Identity) map[string]string {
	return map[string]string{
		scratchtown.ProjectLabel:     scratchtown.Project,
		scratchtown.ConfigFilesLabel: filepath.Join(id.Parent, "l1", "compose.yml"),
	}
}

// withLabel is labels with one key set to value, or without the key when value
// is "".
func withLabel(labels map[string]string, key, value string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		out[k] = v
	}
	if value == "" {
		delete(out, key)
		return out
	}
	out[key] = value
	return out
}

// setLabels is container with its labels replaced.
func setLabels(c scratchtown.Container, labels map[string]string) scratchtown.Container {
	c.Labels = labels
	return c
}

// TestMatchesOnlyTheDocumentedProject pins the identity rule: a container is
// the project's only when the name, the compose project label, the compose file
// under the scratch parent and the image repository all agree, because a name,
// a label or an image alone is something any container can set (gt-4gixm).
func TestMatchesOnlyTheDocumentedProject(t *testing.T) {
	t.Parallel()
	id := testIdentity()
	labels := composeLabels(id)

	cases := []struct {
		name string
		c    scratchtown.Container
		want bool
	}{
		{
			"the project's daemon",
			scratchtown.Container{Name: scratchtown.DaemonContainer, Image: "gt-daemon:4f2a1c9", Labels: labels},
			true,
		},
		{
			"the project's dolt server",
			scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
			true,
		},
		{
			"the project's dind by its pinned name",
			scratchtown.Container{Name: scratchtown.DindContainer, Image: "data.forgejo.org/oci/docker@sha256:7dcdfc4a2024", Labels: labels},
			true,
		},
		{
			"the project's dind by its library name",
			scratchtown.Container{Name: scratchtown.DindContainer, Image: "docker:dind", Labels: labels},
			true,
		},

		{
			"a name spoof with every other condition true",
			scratchtown.Container{Name: "gt-scratch-evil-1", Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
			false,
		},
		{
			"the right name under another compose project",
			setLabels(scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
				withLabel(labels, scratchtown.ProjectLabel, "other")),
			false,
		},
		{
			"the right name and project without a compose file",
			setLabels(scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
				withLabel(labels, scratchtown.ConfigFilesLabel, "")),
			false,
		},
		{
			"a compose file outside the scratch parent",
			setLabels(scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
				withLabel(labels, scratchtown.ConfigFilesLabel,
					filepath.Join(string(filepath.Separator), "home", "op", "elsewhere", "compose.yml"))),
			false,
		},
		{
			"a compose file escaping the scratch parent",
			setLabels(scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
				withLabel(labels, scratchtown.ConfigFilesLabel, filepath.Join(id.Parent, "..", "elsewhere", "compose.yml"))),
			false,
		},
		{
			"a relative compose file",
			setLabels(scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
				withLabel(labels, scratchtown.ConfigFilesLabel, "l1/compose.yml")),
			false,
		},
		{
			"the right name and labels on another image",
			scratchtown.Container{Name: scratchtown.DoltContainer, Image: "someone/dolt-sql-server:2.3.2", Labels: labels},
			false,
		},
		{
			"no labels at all",
			scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2"},
			false,
		},
		{
			"a comma-joined name",
			scratchtown.Container{Name: scratchtown.DoltContainer + ",other", Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := id.Matches(tc.c); got != tc.want {
				t.Errorf("Matches(%s %q) = %v, want %v", tc.c.Image, tc.c.Name, got, tc.want)
			}
		})
	}
}

// TestClassifySeparatesTheProjectFromEveryoneElse: the project's container is a
// Holder while docker is simply running it and Building while the project is
// mid-work, and every other container is Unrelated — an unknown suite, a ryuk
// reaper, a name spoof — so the gate reads it exactly as it does today
// (gt-4gixm).
func TestClassifySeparatesTheProjectFromEveryoneElse(t *testing.T) {
	t.Parallel()
	id := testIdentity()
	labels := composeLabels(id)
	dolt := scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels}

	cases := []struct {
		name string
		c    scratchtown.Container
		want scratchtown.Class
	}{
		{"the project's dolt server, running", scratchtown.Container{Name: dolt.Name, Image: dolt.Image, Labels: labels, State: "running"}, scratchtown.Holder},
		{"the project's dolt server, no state reported", dolt, scratchtown.Holder},
		{"the project's dolt server, created", scratchtown.Container{Name: dolt.Name, Image: dolt.Image, Labels: labels, State: "created"}, scratchtown.Building},
		{"the project's dolt server, restarting", scratchtown.Container{Name: dolt.Name, Image: dolt.Image, Labels: labels, State: "restarting"}, scratchtown.Building},
		{"the project's daemon, paused", scratchtown.Container{Name: scratchtown.DaemonContainer, Image: "gt-daemon:4f2a1c9", Labels: labels, State: "paused"}, scratchtown.Building},

		{
			// A container that names itself the project's (compose project
			// label) and is mid-work is the project mid-work, even with the
			// identity otherwise incomplete.
			"a container the compose project label claims, created",
			scratchtown.Container{Name: "gt-scratch-other-1", Image: "dolthub/dolt-sql-server:2.3.2",
				Labels: withLabel(labels, scratchtown.ConfigFilesLabel, "/somewhere/else/compose.yml"), State: "created"},
			scratchtown.Building,
		},
		{
			"a buildx builder named for the project",
			scratchtown.Container{Name: "buildx_buildkit_gt-scratch0", Image: "moby/buildkit:buildx-stable-1", State: "running"},
			scratchtown.Building,
		},
		{
			"a buildx builder labelled for the project",
			scratchtown.Container{Name: "buildx_buildkit_builder0", Image: "moby/buildkit:buildx-stable-1",
				Labels: map[string]string{"buildx.build.ref": "gt-scratch/buildx_buildkit_builder0"}, State: "running"},
			scratchtown.Building,
		},

		{
			"an unknown dolt container, running",
			scratchtown.Container{Name: "someone-elses-dolt", Image: "dolthub/dolt-sql-server:2.3.2", State: "running"},
			scratchtown.Unrelated,
		},
		{
			"a name spoof, running",
			scratchtown.Container{Name: "gt-scratch-evil-1", Image: "dolthub/dolt-sql-server:2.3.2", Labels: labels, State: "running"},
			scratchtown.Unrelated,
		},
		{
			"a ryuk reaper",
			scratchtown.Container{Name: "reaper", Image: "testcontainers/ryuk:0.5.1", State: "running"},
			scratchtown.Unrelated,
		},
		{
			"a buildx builder for something else",
			scratchtown.Container{Name: "buildx_buildkit_default0", Image: "moby/buildkit:buildx-stable-1", State: "running"},
			scratchtown.Unrelated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := id.Classify(tc.c); got != tc.want {
				t.Errorf("Classify(%s %q, state %q) = %v, want %v", tc.c.Image, tc.c.Name, tc.c.State, got, tc.want)
			}
		})
	}
}

// TestIdentityWithNoParentExemptsNothing: an identity whose parent could not be
// resolved (no home directory) must not exempt a container, or a host it cannot
// read would hand the exemption to anything that names itself the project's.
func TestIdentityWithNoParentExemptsNothing(t *testing.T) {
	t.Parallel()
	c := scratchtown.Container{Name: scratchtown.DoltContainer, Image: "dolthub/dolt-sql-server:2.3.2",
		Labels: composeLabels(testIdentity())}
	if got := (scratchtown.Identity{}).Classify(c); got != scratchtown.Unrelated {
		t.Errorf("Classify with no parent = %v, want %v", got, scratchtown.Unrelated)
	}
}

// TestImageRepository strips the tag and the digest and nothing else: a registry
// port is not a tag (gt-4gixm).
func TestImageRepository(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"gt-daemon:4f2a1c9":                        "gt-daemon",
		"gt-daemon":                                "gt-daemon",
		"dolthub/dolt-sql-server:2.3.2":            "dolthub/dolt-sql-server",
		"data.forgejo.org/oci/docker@sha256:7dcdf": "data.forgejo.org/oci/docker",
		"localhost:5000/some/repo:1.0":             "localhost:5000/some/repo",
	}
	for image, want := range cases {
		if got := scratchtown.ImageRepository(image); got != want {
			t.Errorf("ImageRepository(%q) = %q, want %q", image, got, want)
		}
	}
}
