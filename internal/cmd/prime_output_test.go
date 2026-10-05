package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputRoleDirectives(t *testing.T) {
	t.Parallel()

	t.Run("no directives emits nothing visible", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if strings.Contains(out, "Directives") {
			t.Errorf("expected no header when no directives, got: %s", out)
		}
	})

	t.Run("town-level directive emits town header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "polecat.md"), []byte("Always be polite."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") {
			t.Errorf("expected Town Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Always be polite.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})

	t.Run("rig-level directive emits rig header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "crew.md"), []byte("Watch closely."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RoleCrew,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Rig Directives") {
			t.Errorf("expected Rig Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Watch closely.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})

	t.Run("both levels emits combined header", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "polecat.md"), []byte("Town rule."), 0644); err != nil {
			t.Fatal(err)
		}

		rigDir := filepath.Join(townRoot, "myrig", "directives")
		if err := os.MkdirAll(rigDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigDir, "polecat.md"), []byte("Rig rule."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town & Rig Directives") {
			t.Errorf("expected combined header, got: %s", out)
		}
		if !strings.Contains(out, "Town rule.") {
			t.Errorf("expected town content, got: %s", out)
		}
		if !strings.Contains(out, "Rig rule.") {
			t.Errorf("expected rig content, got: %s", out)
		}
	})

	t.Run("explain mode shows file paths", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		ctx := RoleContext{
			Role:     RolePolecat,
			TownRoot: townRoot,
			Rig:      "myrig",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, true)
		out := buf.String()

		if !strings.Contains(out, "[EXPLAIN]") {
			t.Errorf("expected EXPLAIN output, got: %s", out)
		}
		if !strings.Contains(out, filepath.Join("directives", "polecat.md")) {
			t.Errorf("expected file path in explain output, got: %s", out)
		}
	})

	t.Run("empty rig name skips rig path", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()

		townDir := filepath.Join(townRoot, "directives")
		if err := os.MkdirAll(townDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(townDir, "crew.md"), []byte("Crew directive."), 0644); err != nil {
			t.Fatal(err)
		}

		ctx := RoleContext{
			Role:     RoleCrew,
			TownRoot: townRoot,
			Rig:      "",
		}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") {
			t.Errorf("expected Town Directives header, got: %s", out)
		}
		if !strings.Contains(out, "Crew directive.") {
			t.Errorf("expected directive content, got: %s", out)
		}
	})
}

func TestOutputRoleDirectives_WarnsAboutUnusedFiles(t *testing.T) {
	t.Parallel()

	writeUnusedFixture := func(t *testing.T, roleFile string) string {
		t.Helper()
		townRoot := t.TempDir()
		writeDirective(t, filepath.Join(townRoot, "myrig", "config.json"), "{}")
		if roleFile != "" {
			writeDirective(t, filepath.Join(townRoot, "directives", roleFile+".md"), "Role policy.")
		}
		writeDirective(t, filepath.Join(townRoot, "myrig", "directives", "host-hygiene.md"), "Host rules.")
		// A backup is not a directive; it must not be reported as one.
		writeDirective(t, filepath.Join(townRoot, "myrig", "directives", "refinery.md.bak"), "Old refinery.")
		return townRoot
	}

	// The warning is independent of whether the current role has a directive:
	// the roles most able to fix a misnamed file are the ones that have one.
	t.Run("warns for a role that has its own directive", func(t *testing.T) {
		t.Parallel()
		ctx := RoleContext{Role: RoleCrew, TownRoot: writeUnusedFixture(t, "crew"), Rig: "myrig"}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if !strings.Contains(out, "## Town Directives") || !strings.Contains(out, "Role policy.") {
			t.Errorf("expected the role's own directive, got:\n%s", out)
		}
		if !strings.Contains(out, "Unused Directive Files") {
			t.Errorf("expected the unused-file warning, got:\n%s", out)
		}
		if !strings.Contains(out, "host-hygiene") {
			t.Errorf("expected the misnamed file named, got:\n%s", out)
		}
		if strings.Contains(out, "refinery.md.bak") {
			t.Errorf("a .bak file is not a directive, got:\n%s", out)
		}
	})

	t.Run("warns for a role with no directive of its own", func(t *testing.T) {
		t.Parallel()
		ctx := RoleContext{Role: RolePolecat, TownRoot: writeUnusedFixture(t, ""), Rig: "myrig"}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		out := buf.String()

		if strings.Contains(out, "## Town Directives") {
			t.Errorf("expected no directive header, got:\n%s", out)
		}
		if !strings.Contains(out, "Unused Directive Files") {
			t.Errorf("expected the unused-file warning, got:\n%s", out)
		}
	})

	t.Run("quiet when every file is named for a role", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDirective(t, filepath.Join(townRoot, "directives", "crew.md"), "Crew policy.")
		writeDirective(t, filepath.Join(townRoot, "directives", "polecat.md"), "Polecat policy.")

		ctx := RoleContext{Role: RoleCrew, TownRoot: townRoot, Rig: "myrig"}

		var buf bytes.Buffer
		outputRoleDirectives(ctx, &buf, false)
		if out := buf.String(); strings.Contains(out, "Unused Directive Files") {
			t.Errorf("expected no warning, got:\n%s", out)
		}
	})
}

// TestOutputPrimeContextFallbackDropsTheMayorRole pins the mayor role's
// retirement from the prime fallback (gt-rwp7z.14). A session still carrying
// GT_ROLE=mayor sets this role value and renders the unknown-role brief — the
// directory list sends the agent to a live role — and never a mayor brief.
func TestOutputPrimeContextFallbackDropsTheMayorRole(t *testing.T) {
	t.Parallel()

	ctx := RoleContext{Role: Role("mayor"), TownRoot: t.TempDir(), Rig: "myrig"}

	var buf bytes.Buffer
	outputPrimeContextFallback(&buf, ctx)
	out := buf.String()

	for _, banned := range []string{"Mayor Context", "You are the **Mayor**", "mayor_dispatch", "Idle-Seat Nudges"} {
		if strings.Contains(out, banned) {
			t.Errorf("mayor-role fallback still renders %q:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "Could not determine specific role") {
		t.Errorf("expected the unknown-role brief for the retired mayor role:\n%s", out)
	}
}

// TestOutputUnknownContextListsCrew pins the navigate list to every role
// detectRole resolves from a directory. Crew is live (<rig>/crew/<name>/ maps
// to RoleCrew at role.go), so omitting it sent a lost agent looking for a
// directory the brief never named (gt-uflmc).
func TestOutputUnknownContextListsCrew(t *testing.T) {
	t.Parallel()

	ctx := RoleContext{Role: RoleUnknown, TownRoot: t.TempDir(), Rig: "myrig"}

	var buf bytes.Buffer
	outputUnknownContext(&buf, ctx)
	out := buf.String()

	for _, want := range []string{"<rig>/polecats/<name>/", "<rig>/crew/<name>/"} {
		if !strings.Contains(out, want) {
			t.Errorf("unknown-role context omits navigable dir %q:\n%s", want, out)
		}
	}
}
