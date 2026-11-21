package specsplit

import (
	"reflect"
	"strings"
	"testing"
)

// dirs are the module's package directories the spec tests hand the planner.
var testDirs = []string{"internal/config", "internal/specsplit"}

// TestSplitGroupTwo: bullets and boxes naming two packages come back as two
// groups, each with its own bullets and boxes, the larger group first.
func TestSplitGroupTwo(t *testing.T) {
	t.Parallel()
	spec := Spec{
		Constraints: strings.Join([]string{
			"This paragraph is prose between the bullets and is not a bullet.",
			"- Edit `internal/config/env.go:29` to read the new setting.",
			"- Unlike a URL such as https://go.dev/doc, add internal/specsplit/specsplit.go.",
			"- Move the list parser into internal/specsplit/.",
		}, "\n"),
		Acceptance: strings.Join([]string{
			"- [ ] A test shows the setting. Run: go test ./internal/config -run TestEnv",
			"- [ ] A test shows the parser. Run: go test ./internal/specsplit -run TestSplitGroupTwo",
		}, "\n"),
		Dirs: testDirs,
	}
	got, err := Group(spec)
	if err != nil {
		t.Fatalf("Group() error = %v, want none", err)
	}
	want := []FileGroup{
		{
			Package: "internal/specsplit",
			Constraints: []string{
				"Unlike a URL such as https://go.dev/doc, add internal/specsplit/specsplit.go.",
				"Move the list parser into internal/specsplit/.",
			},
			Boxes: []Box{{
				Text: "A test shows the parser. Run: go test ./internal/specsplit -run TestSplitGroupTwo",
				Run:  "go test ./internal/specsplit -run TestSplitGroupTwo",
			}},
		},
		{
			Package:     "internal/config",
			Constraints: []string{"Edit `internal/config/env.go:29` to read the new setting."},
			Boxes: []Box{{
				Text: "A test shows the setting. Run: go test ./internal/config -run TestEnv",
				Run:  "go test ./internal/config -run TestEnv",
			}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Group() = %+v, want %+v", got, want)
	}
}

// TestSplitGroupReadsOnlyGoTestCommands: a box guarded by a later command is
// placed by its go test alone, so the guard's path is not read as a second
// package.
func TestSplitGroupReadsOnlyGoTestCommands(t *testing.T) {
	t.Parallel()
	spec := Spec{
		Constraints: strings.Join([]string{
			"- Edit internal/config/env.go.",
			"- Add internal/specsplit/specsplit.go.",
		}, "\n"),
		Acceptance: strings.Join([]string{
			"- [ ] The setting is read. Run: go test ./internal/config -run TestEnv; go vet ./...",
			"- [ ] The parser is used. Run: go test ./internal/specsplit -run TestGroup",
		}, "\n"),
		Dirs: testDirs,
	}
	got, err := Group(spec)
	if err != nil {
		t.Fatalf("Group() error = %v, want none", err)
	}
	if len(got) != 2 || got[0].Package != "internal/config" || got[1].Package != "internal/specsplit" {
		t.Fatalf("Group() packages = %+v, want internal/config then internal/specsplit", got)
	}
	if want := "go test ./internal/config -run TestEnv; go vet ./..."; got[0].Boxes[0].Run != want {
		t.Errorf("config box Run = %q, want %q", got[0].Boxes[0].Run, want)
	}
}

// TestSplitGroupRefuses: each bullet, box or spec the planner cannot place is
// an error naming the reason. A one-group spec returns the group with its
// error, since the caller must be able to report what the group held.
func TestSplitGroupRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		spec      Spec
		wantErr   string
		wantGroup []FileGroup
	}{
		{
			name: "a bullet that names no path",
			spec: Spec{
				Constraints: "- Make the documentation better.",
				Acceptance:  "- [ ] A test shows it. Run: go test ./internal/config -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "names no package the spec lists",
		},
		{
			name: "a bullet that names only an absolute path",
			spec: Spec{
				Constraints: "- Edit /home/user/checkout/internal/config/env.go.",
				Acceptance:  "- [ ] A test shows it. Run: go test ./internal/config -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "names no package the spec lists",
		},
		{
			name: "a bullet that names a directory the spec does not list",
			spec: Spec{
				Constraints: "- Edit tools/gen/main.go.",
				Acceptance:  "- [ ] A test shows it. Run: go test ./internal/config -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "names no package the spec lists",
		},
		{
			name: "a box with no Run sentence",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting.",
				Dirs:        testDirs,
			},
			wantErr: "names no go test command",
		},
		{
			name: "a box whose Run command is not go test",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: make gate",
				Dirs:        testDirs,
			},
			wantErr: "names no go test command",
		},
		{
			name: "a box that names a package tree",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./... -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "names a package tree",
		},
		{
			name: "a box that names two packages",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./internal/config ./internal/specsplit -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "names more than one package",
		},
		{
			name: "a bullet that names the root package",
			spec: Spec{
				Constraints: "- Update the root package .",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./internal/config -run TestIt",
				Dirs:        append([]string{"."}, testDirs...),
			},
			wantErr: "names the root package .",
		},
		{
			name: "a box that names the root package",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test . -run TestIt",
				Dirs:        append([]string{"."}, testDirs...),
			},
			wantErr: "names the root package .",
		},
		{
			name: "a box that names a directory the spec does not list",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./tools/gen -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "names no package the spec lists",
		},
		{
			name: "a spec that names no directories",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./internal/config -run TestIt",
			},
			wantErr: "names no package directories",
		},
		{
			name: "a spec with an unclean directory",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./internal/config -run TestIt",
				Dirs:        []string{"internal/config", "internal/../cmd"},
			},
			wantErr: "not a clean repo-relative path",
		},
		{
			name: "a spec that names an absolute directory",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./internal/config -run TestIt",
				Dirs:        []string{"/home/user/checkout/internal/config"},
			},
			wantErr: "not a repo-relative path",
		},
		{
			name: "a spec with one group",
			spec: Spec{
				Constraints: "- Edit internal/config/env.go.",
				Acceptance:  "- [ ] A test shows the setting. Run: go test ./internal/config -run TestIt",
				Dirs:        testDirs,
			},
			wantErr: "one file group internal/config",
			wantGroup: []FileGroup{{
				Package:     "internal/config",
				Constraints: []string{"Edit internal/config/env.go."},
				Boxes: []Box{{
					Text: "A test shows the setting. Run: go test ./internal/config -run TestIt",
					Run:  "go test ./internal/config -run TestIt",
				}},
			}},
		},
		{
			name:    "a spec with no items",
			spec:    Spec{Dirs: testDirs},
			wantErr: "no Constraints bullets or acceptance boxes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Group(tc.spec)
			if err == nil {
				t.Fatalf("Group() error = nil, want one naming %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Group() error = %q, want it to name %q", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.wantGroup) {
				t.Errorf("Group() = %+v, want %+v", got, tc.wantGroup)
			}
		})
	}
}

// TestSplitGroupProseIsNotAPackage: a slash word that is not one of the spec's
// directories names no package. The same bullet, once it names a listed file,
// is placed in that file's directory.
func TestSplitGroupProseIsNotAPackage(t *testing.T) {
	t.Parallel()
	prose := []string{
		"- Call the helper and/or the wrapper.",
		"- Support read/write access to the setting.",
		"- See https://go.dev/doc/env for the setting.",
		"- Backfill rows dated 2026/11/20 before the change.",
	}
	for _, bullet := range prose {
		t.Run(bullet, func(t *testing.T) {
			t.Parallel()
			spec := Spec{
				Constraints: bullet,
				Acceptance:  "- [ ] A test shows it. Run: go test ./internal/config -run TestIt",
				Dirs:        testDirs,
			}
			_, err := Group(spec)
			if err == nil {
				t.Fatalf("Group() error = nil, want an error naming %q", bullet)
			}
			if text := strings.TrimPrefix(bullet, "- "); !strings.Contains(err.Error(), text) {
				t.Errorf("Group() error = %q, want it to name the bullet %q", err, text)
			}
		})
	}
	t.Run("a listed path still places the bullet", func(t *testing.T) {
		t.Parallel()
		bullet := "Support read/write access to the setting and edit internal/config/foo.go."
		spec := Spec{
			Constraints: strings.Join([]string{
				"- " + bullet,
				"- Add internal/specsplit/specsplit.go.",
			}, "\n"),
			Acceptance: strings.Join([]string{
				"- [ ] The setting is read. Run: go test ./internal/config -run TestIt",
				"- [ ] The parser is used. Run: go test ./internal/specsplit -run TestGroup",
			}, "\n"),
			Dirs: testDirs,
		}
		got, err := Group(spec)
		if err != nil {
			t.Fatalf("Group() error = %v, want none", err)
		}
		for _, group := range got {
			if group.Package != "internal/config" {
				continue
			}
			if !reflect.DeepEqual(group.Constraints, []string{bullet}) {
				t.Errorf("internal/config constraints = %q, want only %q", group.Constraints, bullet)
			}
			return
		}
		t.Errorf("Group() = %+v, want a group for internal/config", got)
	})
}
