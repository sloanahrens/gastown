package daemonimage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A path is a canonical clone when its last two components are mayor/rig, and a
// path under one is refused with it. A path that only looks like one is not.
func TestIsCanonicalClone(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/town/gastown/mayor/rig":      true,
		"/town/gastown/mayor/rig/sub":  true,
		"/town/gastown/crew/seagull":   false,
		"/town/gastown/mayor":          false,
		"/town/rig":                    false,
		"/elsewhere/mayor/rigs":        false,
		"/":                            false,
		"/town/gastown/refinery/rig":   false,
		"/scratch.noindex/l1/build":    false,
		"/town/gastown/mayor/rig/a/b/": true,
	} {
		if got := IsCanonicalClone(path); got != want {
			t.Errorf("IsCanonicalClone(%q) = %v, want %v", path, got, want)
		}
	}
}

// The Builder refuses a canonical clone, or a path inside one, as any input a
// build reads or writes: the output directory and each source. A symlink to the
// clone is refused as the clone; a sibling that only looks like one is not.
// Plan refuses before it prints anything, BuildBinaries before it runs a command,
// and BuildImage refuses its build context before docker runs.
func TestBuilderRefusesACanonicalCloneAsAnInput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	clone := filepath.Join(root, "town", "gastown", "mayor", "rig")
	inside := filepath.Join(clone, "sub")
	sibling := filepath.Join(root, "town", "gastown", "mayor", "rigs")
	link := filepath.Join(root, "link")
	for _, dir := range []string{inside, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(clone, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	inputs := []struct {
		name string
		set  func(b *Builder, dir string)
	}{
		{"the output directory", func(b *Builder, dir string) { b.Out = dir }},
		{"the gt source", func(b *Builder, dir string) { b.GTSrc = dir }},
		{"the bd source", func(b *Builder, dir string) { b.BDSrc = dir }},
		{"the om source", func(b *Builder, dir string) { b.OMSrc = dir }},
	}
	for _, p := range []struct {
		name    string
		dir     string
		refused bool
	}{
		{"the clone", clone, true},
		{"a directory inside the clone", inside, true},
		{"a symlink to the clone", link, true},
		{"a sibling that is not a clone", sibling, false},
	} {
		for _, in := range inputs {
			t.Run(in.name+" is "+p.name, func(t *testing.T) {
				t.Parallel()
				b := planBuilder(t)
				in.set(&b, p.dir)
				lines, err := b.Plan(testRev)
				if !p.refused {
					if err != nil {
						t.Fatalf("Plan refused %s: %v", p.dir, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "canonical clone") {
					t.Fatalf("Plan = %q, %v; want a refusal naming the canonical clone", lines, err)
				}
				if len(lines) != 0 {
					t.Errorf("a refused plan listed %q", lines)
				}
				b.Run = func(context.Context, string, []string, []string) error {
					t.Error("a refused build ran a command")
					return nil
				}
				b.Git = func(string, ...string) ([]byte, error) {
					t.Error("a refused build ran git")
					return nil, nil
				}
				if _, err := b.BuildBinaries(context.Background(), testRev); err == nil || !strings.Contains(err.Error(), "canonical clone") {
					t.Errorf("BuildBinaries = %v; want a refusal naming the canonical clone", err)
				}
			})
		}
		t.Run("the build context is "+p.name, func(t *testing.T) {
			t.Parallel()
			b := planBuilder(t)
			b.Out = p.dir
			b.Run = func(context.Context, string, []string, []string) error {
				t.Error("a refused image build ran docker")
				return nil
			}
			_, err := b.BuildImage(context.Background(), testRev, nil)
			if !p.refused {
				if err != nil && strings.Contains(err.Error(), "canonical clone") {
					t.Fatalf("BuildImage refused %s: %v", p.dir, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "canonical clone") {
				t.Errorf("BuildImage = %v; want a refusal naming the canonical clone", err)
			}
		})
	}
}
