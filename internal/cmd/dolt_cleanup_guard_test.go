package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestAgentActor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"human terminal has no actor", nil, ""},
		{"GT_ROLE wins", map[string]string{"GT_ROLE": "deacon/dogs/alpha", "BD_ACTOR": "deacon-alpha"}, "deacon/dogs/alpha"},
		{"BD_ACTOR fallback", map[string]string{"BD_ACTOR": "deacon-alpha"}, "deacon-alpha"},
	} {
		getenv := func(k string) string { return tc.env[k] }
		if actor := agentActor(getenv); actor != tc.want {
			t.Errorf("%s: agentActor = %q, want %q", tc.name, actor, tc.want)
		}
	}
}

func TestCheckAgentForceAuthorization(t *testing.T) {
	t.Parallel()
	t.Run("refuses without authorization bead", func(t *testing.T) {
		err := checkAgentForceAuthorization("deacon/dogs/alpha", "")
		if err == nil {
			t.Fatal("expected refusal for agent --force without --authorized-by")
		}
		if !strings.Contains(err.Error(), "gt-61x") {
			t.Errorf("error should reference the guardrail bead, got: %v", err)
		}
		if !strings.Contains(err.Error(), "--authorized-by") {
			t.Errorf("error should explain the remedy, got: %v", err)
		}
	})

	t.Run("allows with authorization bead", func(t *testing.T) {
		if err := checkAgentForceAuthorization("deacon/dogs/alpha", "hq-abc"); err != nil {
			t.Errorf("expected authorization to pass, got: %v", err)
		}
	})
}

func TestHoldsGateError(t *testing.T) {
	t.Parallel()
	t.Run("no error passes", func(t *testing.T) {
		if err := holdsGateError(nil, false); err != nil {
			t.Errorf("expected nil for successful holds query, got: %v", err)
		}
	})

	t.Run("dry run continues despite holds failure", func(t *testing.T) {
		if err := holdsGateError(errors.New("dolt down"), true); err != nil {
			t.Errorf("dry run is non-destructive and should continue, got: %v", err)
		}
	})

	t.Run("destructive run fails closed on holds failure", func(t *testing.T) {
		err := holdsGateError(errors.New("dolt down"), false)
		if err == nil {
			t.Fatal("expected fail-closed error when holds cannot be verified for a destructive run")
		}
		if !strings.Contains(err.Error(), "holds") {
			t.Errorf("error should mention holds, got: %v", err)
		}
	})
}

// testAuditor builds a cleanupAuditor whose bead and event writes are captured
// in-memory. beadErr simulates AddComment failure; beadID "" means no
// authorizing bead (human forced run).
func testAuditor(beadID string, beadErr error) (*cleanupAuditor, *[]string, *[]string) {
	var comments, eventTypes []string
	a := &cleanupAuditor{
		actor:  "gastown/polecats/onyx",
		beadID: beadID,
		addComment: func(id, text string) error {
			if beadErr != nil {
				return beadErr
			}
			comments = append(comments, id+": "+text)
			return nil
		},
		logAudit: func(eventType string, payload map[string]interface{}) {
			eventTypes = append(eventTypes, eventType)
		},
	}
	return a, &comments, &eventTypes
}

func TestCleanupAuditorIntent(t *testing.T) {
	t.Parallel()
	t.Run("writes intent to bead before any removal", func(t *testing.T) {
		a, comments, events := testAuditor("hq-abc", nil)
		if err := a.recordIntent([]string{"testdb_1", "testdb_2"}); err != nil {
			t.Fatalf("expected intent to succeed, got: %v", err)
		}
		if len(*comments) != 1 {
			t.Fatalf("expected 1 bead comment, got %d", len(*comments))
		}
		c := (*comments)[0]
		for _, want := range []string{"hq-abc", "INTENT", "gastown/polecats/onyx", "testdb_1", "testdb_2"} {
			if !strings.Contains(c, want) {
				t.Errorf("intent comment missing %q: %s", want, c)
			}
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event, got %d", len(*events))
		}
	})

	t.Run("fails closed when intent cannot be recorded", func(t *testing.T) {
		a, _, _ := testAuditor("hq-abc", errors.New("dolt down"))
		err := a.recordIntent([]string{"testdb_1"})
		if err == nil {
			t.Fatal("expected fail-closed error when intent record cannot be written (gt-87a)")
		}
		if !strings.Contains(err.Error(), "gt-87a") {
			t.Errorf("error should reference the guardrail bead, got: %v", err)
		}
	})

	t.Run("human run without bead still logs an audit event", func(t *testing.T) {
		a, comments, events := testAuditor("", nil)
		if err := a.recordIntent([]string{"testdb_1"}); err != nil {
			t.Fatalf("no-bead intent must not fail, got: %v", err)
		}
		if len(*comments) != 0 {
			t.Errorf("expected no bead comments without an authorizing bead, got %d", len(*comments))
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event for human forced run, got %d", len(*events))
		}
	})
}

func TestCleanupAuditorCompletion(t *testing.T) {
	t.Parallel()
	t.Run("records completion with counts and names", func(t *testing.T) {
		a, comments, _ := testAuditor("hq-abc", nil)
		if err := a.recordCompletion(2, 3, []string{"testdb_1", "testdb_2"}); err != nil {
			t.Fatalf("expected completion to succeed, got: %v", err)
		}
		if len(*comments) != 1 {
			t.Fatalf("expected 1 bead comment, got %d", len(*comments))
		}
		c := (*comments)[0]
		for _, want := range []string{"removed 2/3", "testdb_1", "testdb_2", "gastown/polecats/onyx"} {
			if !strings.Contains(c, want) {
				t.Errorf("completion comment missing %q: %s", want, c)
			}
		}
	})

	t.Run("records even when nothing was removed", func(t *testing.T) {
		a, comments, events := testAuditor("hq-abc", nil)
		if err := a.recordCompletion(0, 2, nil); err != nil {
			t.Fatalf("failed-attempt completion must still record, got: %v", err)
		}
		if len(*comments) != 1 {
			t.Fatalf("expected a bead comment for a failed attempt (removed==0), got %d", len(*comments))
		}
		if !strings.Contains((*comments)[0], "removed 0/2") {
			t.Errorf("failed-attempt comment should show 0/2, got: %s", (*comments)[0])
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event, got %d", len(*events))
		}
	})

	t.Run("recording failure is loud", func(t *testing.T) {
		a, _, _ := testAuditor("hq-abc", errors.New("dolt down"))
		err := a.recordCompletion(2, 2, []string{"testdb_1", "testdb_2"})
		if err == nil {
			t.Fatal("expected non-nil error when completion record fails (gt-87a: no silent exit 0)")
		}
		if !strings.Contains(err.Error(), "hq-abc") {
			t.Errorf("error should name the authorizing bead, got: %v", err)
		}
	})

	t.Run("human run without bead logs event only", func(t *testing.T) {
		a, comments, events := testAuditor("", nil)
		if err := a.recordCompletion(1, 1, []string{"testdb_1"}); err != nil {
			t.Fatalf("no-bead completion must not fail, got: %v", err)
		}
		if len(*comments) != 0 {
			t.Errorf("expected no bead comments without an authorizing bead, got %d", len(*comments))
		}
		if len(*events) != 1 {
			t.Errorf("expected 1 audit event, got %d", len(*events))
		}
	})
}

func TestResolveDestructiveActor(t *testing.T) {
	t.Parallel()
	t.Run("env identity is an agent", func(t *testing.T) {
		actor, isAgent := resolveDestructiveActor("gastown/polecats/onyx", true)
		if !isAgent || actor != "gastown/polecats/onyx" {
			t.Errorf("expected agent with env identity, got actor=%q isAgent=%v", actor, isAgent)
		}
	})

	t.Run("no identity at a terminal is a human", func(t *testing.T) {
		actor, isAgent := resolveDestructiveActor("", true)
		if isAgent || actor != "" {
			t.Errorf("expected human operator, got actor=%q isAgent=%v", actor, isAgent)
		}
	})

	t.Run("no identity without a terminal is agent-by-default", func(t *testing.T) {
		actor, isAgent := resolveDestructiveActor("", false)
		if !isAgent {
			t.Error("unset identity off-terminal must be treated as an agent (spoofing guard, gt-2oy)")
		}
		if actor != doltserver.UnidentifiedActor {
			t.Errorf("agent-by-default actor = %q, want doltserver.UnidentifiedActor", actor)
		}
	})
}

// TestAuthorizeCleanupForceRefusesUnidentifiedActor covers the guard helper:
// with no env identity and no terminal, --force must fail closed before the
// authorization bead is looked up, so no database can be removed (gt-kz9k0).
func TestAuthorizeCleanupForceRefusesUnidentifiedActor(t *testing.T) {
	t.Parallel()
	err := authorizeCleanupForce(t.TempDir(), "", false, "hq-abc")
	if err == nil {
		t.Fatal("expected refusal for an unidentified actor")
	}
	if !errors.Is(err, doltserver.ErrUnidentifiedActor) {
		t.Errorf("error = %v, want ErrUnidentifiedActor (refusal must precede the bead lookup)", err)
	}
}

// TestRunDoltCleanupForceRefusesUnidentifiedActor drives the command itself:
// with GT_ROLE/BD_ACTOR unset and stdout not a terminal, `gt dolt cleanup
// --force --authorized-by hq-abc` must refuse before it discovers or removes
// any database. Calling authorizeCleanupForce directly cannot catch the guard
// being dropped from this call site.
//
//testpolicy:allow parallel — runDoltCleanup reads process-wide state (flags, environment, stdout, cwd)
func TestRunDoltCleanupForceRefusesUnidentifiedActor(t *testing.T) {
	// Own everything the command resolves: a sandbox town with no .dolt-data,
	// so a dropped guard finds nothing to remove, and a temp cwd that keeps
	// any real town above the test out of the walk-up search.
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — runDoltCleanup resolves the town root by walking up from the process cwd
	t.Chdir(t.TempDir())
	//testpolicy:allow no-env — GT_TOWN_ROOT is the fallback the command reads when the cwd is outside a town
	t.Setenv("GT_TOWN_ROOT", townRoot)
	//testpolicy:allow no-env — GT_ROLE is the identity the command reads via agentActor
	t.Setenv("GT_ROLE", "")
	//testpolicy:allow no-env — BD_ACTOR is the fallback identity the command reads
	t.Setenv("BD_ACTOR", "")

	// stdout must not be a terminal, or the command classifies the actor as a
	// human operator and correctly lets the force through.
	stdout := os.Stdout
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-global-swap — ui.IsTerminal reads os.Stdout inside runDoltCleanup; it is not an argument
	os.Stdout = devNull
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the os.Stdout swapped above
		os.Stdout = stdout
		devNull.Close()
	})

	// The cleanup flags are package-level and bound to the shared command tree.
	force, dry, authorizedBy := doltCleanupForce, doltCleanupDry, doltCleanupAuthorizedBy
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the flag variables swapped below
		doltCleanupForce, doltCleanupDry, doltCleanupAuthorizedBy = force, dry, authorizedBy
	})
	//testpolicy:allow no-global-swap — runDoltCleanup reads the --force, --dry-run and --authorized-by flag variables; they are not arguments
	doltCleanupForce, doltCleanupDry, doltCleanupAuthorizedBy = true, false, "hq-abc"

	err = runDoltCleanup(nil, nil)
	if err == nil {
		t.Fatal("runDoltCleanup with --force and an unidentified actor = nil, want refusal")
	}
	if !errors.Is(err, doltserver.ErrUnidentifiedActor) {
		t.Errorf("error = %v, want ErrUnidentifiedActor (refusal must precede orphan discovery and removal)", err)
	}
}
