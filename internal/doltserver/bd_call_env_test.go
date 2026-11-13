package doltserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The bd calls that read and write a rig's issue_prefix take the town's named
// Dolt endpoint, whatever the inherited environment holds. The rig's beads dir
// sits outside the town tree, where bd cannot find the town to apply the
// endpoint itself, so an inherited port would decide where bd dials (gt-xnv94x).
func TestRigIssuePrefixCallsTakeTheTownEndpoint(t *testing.T) {
	t.Parallel()
	namedTown := t.TempDir()
	writeTownWithDolt(t, namedTown, `,"dolt":{"host":"127.0.0.1","port":4407}`)
	unnamedTown := t.TempDir()
	writeTownWithDolt(t, unnamedTown, "")
	read := func(h *host, townRoot, beadsDir string) error {
		_, err := h.readRigIssuePrefix(townRoot, beadsDir)
		return err
	}
	write := func(h *host, townRoot, beadsDir string) error {
		return h.writeRigIssuePrefix(townRoot, beadsDir, "forkrig", "fr")
	}

	for _, tc := range []struct {
		name     string
		call     func(h *host, townRoot, beadsDir string) error
		townRoot string
		wantPort string
	}{
		{"read takes the town's endpoint", read, namedTown, "4407"},
		{"write takes the town's endpoint", write, namedTown, "4407"},
		{"read keeps the inherited port when the town names none", read, unnamedTown, "1"},
		{"write keeps the inherited port when the town names none", write, unnamedTown, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen [][]string
			h := &host{
				environList: func() []string {
					return []string{"PATH=/usr/bin", "BEADS_DOLT_PORT=1", "BEADS_DOLT_SERVER_PORT=1"}
				},
				bdRunner: func(_ context.Context, _ string, env, _ []string) ([]byte, []byte, error) {
					seen = append(seen, env)
					return nil, nil, nil
				},
			}
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := tc.call(h, tc.townRoot, beadsDir); err != nil {
				t.Fatalf("bd call: %v", err)
			}
			if len(seen) == 0 {
				t.Fatal("no bd call reached the runner")
			}
			for _, env := range seen {
				if got, _ := lastEnvValue(env, "BEADS_DOLT_PORT"); got != tc.wantPort {
					t.Errorf("bd BEADS_DOLT_PORT = %q, want %q", got, tc.wantPort)
				}
			}
		})
	}
}

// A failed rename-prefix names the reason bd wrote in its error envelope, not
// its permissions warning, and the error still matches beads.ErrUnavailable
// (gt-xnv94x).
func TestWriteRigIssuePrefixNamesBDsReason(t *testing.T) {
	t.Parallel()
	envelope := "{\"contract_version\":1,\"data\":null,\"error\":{\"kind\":\"store_unavailable\",\"message\":\"failed to open database \\\"forkrig\\\"\"}}\n"
	h := &host{
		environList: func() []string { return []string{"PATH=/usr/bin"} },
		bdRunner: func(_ context.Context, _ string, _, _ []string) ([]byte, []byte, error) {
			stderr := []byte("Warning: .beads has permissions 0755 (recommended: 0700)\n")
			return []byte(envelope), stderr, errors.New("exit status 1")
		},
	}
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := h.writeRigIssuePrefix(t.TempDir(), beadsDir, "forkrig", "fr")
	if want := `setting issue_prefix for forkrig: failed to open database "forkrig"`; err == nil || err.Error() != want {
		t.Fatalf("writeRigIssuePrefix() = %v, want %q", err, want)
	}
	if !errors.Is(err, beads.ErrUnavailable) {
		t.Fatalf("writeRigIssuePrefix() lost ErrUnavailable: %v", err)
	}
}

// writeTownWithDolt writes mayor/town.json under root. extra is appended to the
// town object, so it carries the town's dolt block or nothing.
func writeTownWithDolt(t *testing.T, root, extra string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	town := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"` + extra + `}`
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(town), 0o600); err != nil {
		t.Fatal(err)
	}
}
