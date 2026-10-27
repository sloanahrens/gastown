package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRoleDirective(t *testing.T) {
	t.Parallel()

	t.Run("town-level only", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "polecat.md"), []byte("town directive"), 0644); err != nil {
			t.Fatal(err)
		}

		got := LoadRoleDirective("polecat", townRoot, "myrig")
		if got != "town directive" {
			t.Errorf("got %q, want %q", got, "town directive")
		}
	})

	t.Run("rig-level only", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		rigDir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, "crew.md"), []byte("rig directive"), 0644); err != nil {
			t.Fatal(err)
		}

		got := LoadRoleDirective("crew", townRoot, "myrig")
		if got != "rig directive" {
			t.Errorf("got %q, want %q", got, "rig directive")
		}
	})

	t.Run("both levels concatenated with rig last", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "polecat.md"), []byte("town rules"), 0644); err != nil {
			t.Fatal(err)
		}

		rigDir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, "polecat.md"), []byte("rig rules"), 0644); err != nil {
			t.Fatal(err)
		}

		got := LoadRoleDirective("polecat", townRoot, "myrig")
		want := "town rules\nrig rules"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no directives returns empty", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		got := LoadRoleDirective("polecat", townRoot, "myrig")
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("invalid paths graceful", func(t *testing.T) {
		t.Parallel()

		// Non-existent town root
		got := LoadRoleDirective("polecat", "/nonexistent/path/xyz", "myrig")
		if got != "" {
			t.Errorf("got %q, want empty string for invalid town root", got)
		}

		// Empty rig name skips rig-level lookup
		townRoot := t.TempDir()
		rigDir := filepath.Join(townRoot, "", "directives")
		// With empty rigName, this path would be townRoot/directives — same as town-level
		// Verify it doesn't panic or error
		_ = rigDir
		got = LoadRoleDirective("polecat", townRoot, "")
		if got != "" {
			t.Errorf("got %q, want empty string for empty rig", got)
		}
	})

	t.Run("whitespace-only file treated as absent", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "polecat.md"), []byte("  \n\t\n  "), 0644); err != nil {
			t.Fatal(err)
		}

		got := LoadRoleDirective("polecat", townRoot, "myrig")
		if got != "" {
			t.Errorf("got %q, want empty string for whitespace-only directive", got)
		}
	})
}

func TestLoadRoleDirectiveShared(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("shared file renders for a role that has no file of its own", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		write(t, filepath.Join(townRoot, "directives", SharedDirectiveName+".md"), "host rules")

		got := LoadRoleDirective("polecat", townRoot, "myrig")
		if got != "host rules" {
			t.Errorf("got %q, want %q", got, "host rules")
		}
	})

	t.Run("every level concatenates broadest first", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		write(t, filepath.Join(townRoot, "directives", SharedDirectiveName+".md"), "town shared")
		write(t, filepath.Join(townRoot, "myrig", "directives", SharedDirectiveName+".md"), "rig shared")
		write(t, filepath.Join(townRoot, "directives", "polecat.md"), "town role")
		write(t, filepath.Join(townRoot, "myrig", "directives", "polecat.md"), "rig role")

		got := LoadRoleDirective("polecat", townRoot, "myrig")
		want := "town shared\nrig shared\ntown role\nrig role"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("asking for the shared name yields it once", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		write(t, filepath.Join(townRoot, "directives", SharedDirectiveName+".md"), "town shared")
		write(t, filepath.Join(townRoot, "myrig", "directives", SharedDirectiveName+".md"), "rig shared")

		got := LoadRoleDirective(SharedDirectiveName, townRoot, "myrig")
		want := "town shared\nrig shared"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("an empty rig name skips the rig copies", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		write(t, filepath.Join(townRoot, "directives", SharedDirectiveName+".md"), "town shared")
		write(t, filepath.Join(townRoot, "myrig", "directives", SharedDirectiveName+".md"), "rig shared")

		got := LoadRoleDirective("polecat", townRoot, "")
		if got != "town shared" {
			t.Errorf("got %q, want %q", got, "town shared")
		}
	})
}

// TestLoadRoleDirectiveSkipsNamesForNoKnownRole pins the contract the design
// doc and gt directive list state: only a name a role has renders, so a typo
// or a retired role loads the shared files and nothing else (gt-r3jbi).
func TestLoadRoleDirectiveSkipsNamesForNoKnownRole(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, role := range []string{"refiner", "witness", "unknown"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			write(t, filepath.Join(townRoot, "directives", SharedDirectiveName+".md"), "host rules")
			write(t, filepath.Join(townRoot, "directives", role+".md"), "misnamed central policy")
			write(t, filepath.Join(townRoot, "myrig", "directives", role+".md"), "misnamed rig policy")

			if got := LoadRoleDirective(role, townRoot, "myrig"); got != "host rules" {
				t.Errorf("LoadRoleDirective(%q) = %q, want only the shared file", role, got)
			}
			// The system prompt reads through the strict reader instead.
			dirs, err := ReadRoleDirectives(role, townRoot, "myrig")
			if err != nil {
				t.Fatalf("ReadRoleDirectives: %v", err)
			}
			if len(dirs) != 1 || dirs[0].Name != SharedDirectiveName {
				t.Errorf("ReadRoleDirectives(%q) = %+v, want only the shared file", role, dirs)
			}
		})
	}
}

func TestIsKnownRole(t *testing.T) {
	t.Parallel()

	for _, role := range AllRoles() {
		if !IsKnownRole(role) {
			t.Errorf("IsKnownRole(%q) = false, want true", role)
		}
	}
	for _, name := range []string{SharedDirectiveName, "host-hygiene", "testing", "", "Polecat"} {
		if IsKnownRole(name) {
			t.Errorf("IsKnownRole(%q) = true, want false", name)
		}
	}
}

func TestIsTownLevelDir(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"mayor", "deacon", ".beads", ".claude", ".git", "plugins"} {
		if !IsTownLevelDir(name) {
			t.Errorf("IsTownLevelDir(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"myrig", "gastown", "directives", "Mayor", ".github", ""} {
		if IsTownLevelDir(name) {
			t.Errorf("IsTownLevelDir(%q) = true, want false", name)
		}
	}
}

func TestRigNamesToScanExcludesTownAgentDirs(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	// Town-level dirs can carry a config.json of their own, so only the dir
	// name separates them from a real rig.
	for _, dir := range []string{"mayor", "deacon", ".beads", ".claude", ".git", "plugins", "myrig"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townRoot, dir, "config.json"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("every rig scan", func(t *testing.T) {
		t.Parallel()
		got, err := rigNamesToScan(townRoot, "")
		if err != nil {
			t.Fatalf("rigNamesToScan: %v", err)
		}
		if want := []string{"myrig"}; !equalStrings(got, want) {
			t.Errorf("rigNamesToScan = %v, want %v", got, want)
		}
	})

	t.Run("a named rig is kept", func(t *testing.T) {
		t.Parallel()
		got, err := rigNamesToScan(townRoot, "myrig")
		if err != nil {
			t.Fatalf("rigNamesToScan: %v", err)
		}
		if want := []string{"myrig"}; !equalStrings(got, want) {
			t.Errorf("rigNamesToScan = %v, want %v", got, want)
		}
	})

	t.Run("a named town-level dir is not a rig", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"mayor", "deacon", ".beads", ".claude", ".git", "plugins"} {
			got, err := rigNamesToScan(townRoot, name)
			if err != nil {
				t.Fatalf("rigNamesToScan(%q): %v", name, err)
			}
			if len(got) != 0 {
				t.Errorf("rigNamesToScan(%q) = %v, want none", name, got)
			}
		}
	})
}

func TestScanDirectiveFiles(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// A town with a real role file, the shared file, two misnamed files, and a
	// non-.md file that must be ignored.
	newTown := func(t *testing.T) string {
		t.Helper()
		townRoot := t.TempDir()
		write(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
		write(t, filepath.Join(townRoot, "directives", "crew.md"), "crew policy")
		write(t, filepath.Join(townRoot, "directives", SharedDirectiveName+".md"), "shared policy")
		write(t, filepath.Join(townRoot, "myrig", "directives", "polecat.md"), "rig polecat")
		write(t, filepath.Join(townRoot, "myrig", "directives", "host-hygiene.md"), "host rules")
		write(t, filepath.Join(townRoot, "myrig", "directives", "testing.md"), "test rules")
		write(t, filepath.Join(townRoot, "myrig", "directives", "polecat.md.bak"), "backup")
		return townRoot
	}

	unusedRoles := func(files []DirectiveFile) []string {
		var got []string
		for _, f := range files {
			if f.Unused() {
				got = append(got, f.Role)
			}
		}
		return got
	}

	t.Run("named rig scans town and that rig only", func(t *testing.T) {
		t.Parallel()
		files, err := ScanDirectiveFiles(newTown(t), "myrig")
		if err != nil {
			t.Fatalf("ScanDirectiveFiles: %v", err)
		}

		if got, want := unusedRoles(files), []string{"host-hygiene", "testing"}; !equalStrings(got, want) {
			t.Errorf("unused roles = %v, want %v", got, want)
		}
		for _, f := range files {
			if f.Path == "" || !filepath.IsAbs(f.Path) {
				t.Errorf("entry %+v has no absolute path", f)
			}
		}
	})

	t.Run("the shared name is never unused", func(t *testing.T) {
		t.Parallel()
		files, err := ScanDirectiveFiles(newTown(t), "myrig")
		if err != nil {
			t.Fatalf("ScanDirectiveFiles: %v", err)
		}

		var sawShared bool
		for _, f := range files {
			if f.Role == SharedDirectiveName {
				sawShared = true
				if f.Unused() {
					t.Errorf("shared file %s reported as unused", f.Path)
				}
			}
		}
		if !sawShared {
			t.Error("shared file missing from scan")
		}
	})

	t.Run("empty rig name scans every rig with a config.json", func(t *testing.T) {
		t.Parallel()
		townRoot := newTown(t)
		write(t, filepath.Join(townRoot, "otherrig", "config.json"), "{}")
		write(t, filepath.Join(townRoot, "otherrig", "directives", "typod.md"), "other policy")
		// A town directory without config.json is not a rig.
		write(t, filepath.Join(townRoot, "notarig", "directives", "stray.md"), "stray")

		files, err := ScanDirectiveFiles(townRoot, "")
		if err != nil {
			t.Fatalf("ScanDirectiveFiles: %v", err)
		}

		if got, want := unusedRoles(files), []string{"host-hygiene", "testing", "typod"}; !equalStrings(got, want) {
			t.Errorf("unused roles = %v, want %v", got, want)
		}
	})

	t.Run("a named town-level dir contributes no rig files", func(t *testing.T) {
		t.Parallel()
		townRoot := newTown(t)
		write(t, filepath.Join(townRoot, "mayor", "directives", "stale.md"), "mayor policy")

		files, err := ScanDirectiveFiles(townRoot, "mayor")
		if err != nil {
			t.Fatalf("ScanDirectiveFiles: %v", err)
		}
		for _, f := range files {
			if f.Scope == "mayor" {
				t.Errorf("town-level dir scanned as a rig: %+v", f)
			}
		}
		if len(files) != 2 {
			t.Errorf("got %d files, want the 2 town files", len(files))
		}
	})

	t.Run("a missing directives directory is not an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		write(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")

		files, err := ScanDirectiveFiles(townRoot, "myrig")
		if err != nil {
			t.Fatalf("ScanDirectiveFiles: %v", err)
		}
		if len(files) != 0 {
			t.Errorf("got %d files, want 0", len(files))
		}
	})

	t.Run("an unreadable directives directory is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		// A file where the directory should be makes ReadDir fail with
		// something other than ErrNotExist.
		write(t, filepath.Join(townRoot, "directives"), "not a directory")

		if _, err := ScanDirectiveFiles(townRoot, "myrig"); err == nil {
			t.Error("expected an error when the directives directory cannot be read")
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
