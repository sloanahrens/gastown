package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// stubRigPolecatSessions is a rigPolecatSessions with canned answers that
// counts StopAll calls.
type stubRigPolecatSessions struct {
	infos    []polecat.SessionInfo
	listErr  error
	stopErr  error
	stopCall int
}

func (s *stubRigPolecatSessions) ListPolecats() ([]polecat.SessionInfo, error) {
	return s.infos, s.listErr
}

func (s *stubRigPolecatSessions) StopAll(bool) error {
	s.stopCall++
	return s.stopErr
}

func (s *stubRigPolecatSessions) open(*rig.Rig, string, string) rigPolecatSessions { return s }

// registeredRigTown writes a town at a temp dir with one registered rig, "testrig", and
// returns its root.
func registeredRigTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"testrig": {GitURL: "git@github.com:test/testrig.git", AddedAt: time.Now().Truncate(time.Second)},
		},
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), rigsConfig); err != nil {
		t.Fatalf("save rigs.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "testrig"), 0o755); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// TestRunRigShutdownListPolecatsError: a polecat session list that cannot be
// read is a failed shutdown and stop, not an empty rig. Both used to skip the
// stop block and report success while the sessions still ran (gt-ev685).
func TestRunRigShutdownListPolecatsError(t *testing.T) {
	t.Parallel()
	listErr := errors.New("tmux server unreachable")
	infos := []polecat.SessionInfo{{Polecat: "furiosa", Running: true}}

	tests := []struct {
		name     string
		stub     stubRigPolecatSessions
		wantErr  bool
		wantStop int
	}{
		{"list error", stubRigPolecatSessions{listErr: listErr}, true, 0},
		{"stop error", stubRigPolecatSessions{infos: infos, stopErr: errors.New("kill failed")}, true, 1},
		{"no sessions", stubRigPolecatSessions{}, false, 0},
		{"stopped", stubRigPolecatSessions{infos: infos}, false, 1},
	}
	for _, tt := range tests {
		t.Run("shutdown/"+tt.name, func(t *testing.T) {
			t.Parallel()
			stub := tt.stub // a copy: shutdown and stop run in parallel
			err := runRigShutdownIn(registeredRigTown(t), "testrig", false, true, stub.open)
			if (err != nil) != tt.wantErr {
				t.Errorf("runRigShutdownIn err = %v, wantErr %v", err, tt.wantErr)
			}
			if stub.stopCall != tt.wantStop {
				t.Errorf("StopAll calls = %d, want %d", stub.stopCall, tt.wantStop)
			}
		})
		t.Run("stop/"+tt.name, func(t *testing.T) {
			t.Parallel()
			stub := tt.stub
			err := runRigStopIn(registeredRigTown(t), []string{"testrig"}, false, true, stub.open)
			if (err != nil) != tt.wantErr {
				t.Errorf("runRigStopIn err = %v, wantErr %v", err, tt.wantErr)
			}
			if stub.stopCall != tt.wantStop {
				t.Errorf("StopAll calls = %d, want %d", stub.stopCall, tt.wantStop)
			}
		})
	}
}

// TestRunRigListLoadError: a rigs.json that cannot be parsed is an error, not
// "No rigs configured." with exit 0, and a --json run writes no text. A
// missing rigs.json is an empty town, and --json then prints a valid empty
// list (gt-ev685).
func TestRunRigListLoadError(t *testing.T) {
	t.Parallel()

	for _, asJSON := range []bool{false, true} {
		t.Run("malformed", func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runRigListIn(corruptRigsTown(t), &out, asJSON)
			if err == nil {
				t.Fatal("runRigListIn with a malformed rigs.json = nil, want an error")
			}
			if !strings.Contains(err.Error(), "rigs config") {
				t.Errorf("err = %v, want it to name the rigs config", err)
			}
			if strings.Contains(out.String(), "No rigs configured.") {
				t.Errorf("output = %q, want no \"No rigs configured.\" text", out.String())
			}
		})
	}

	t.Run("missing json is an empty list", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := runRigListIn(t.TempDir(), &out, true); err != nil {
			t.Fatalf("runRigListIn: %v", err)
		}
		if got := strings.TrimSpace(out.String()); got != "[]" {
			t.Errorf("--json output = %q, want []", got)
		}
	})

	t.Run("missing text says no rigs", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := runRigListIn(t.TempDir(), &out, false); err != nil {
			t.Fatalf("runRigListIn: %v", err)
		}
		if !strings.Contains(out.String(), "No rigs configured.") {
			t.Errorf("output = %q, want \"No rigs configured.\"", out.String())
		}
	})
}

func TestIsGitRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		// Remote URLs — should return true
		{"https://github.com/org/repo.git", true},
		{"http://github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", true},
		{"git://github.com/org/repo.git", true},
		{"deploy@private-host.internal:repos/app.git", true},

		// Custom git remote helper schemes — should return true
		{"s3://my-bucket/rigs/my-project", true},
		{"codecommit://my-repo", true},
		{"gs://my-bucket/repos/foo", true},

		// Local paths — should return false
		{"/Users/scott/projects/foo", false},
		{"/tmp/repo", false},
		{"./foo", false},
		{"../foo", false},
		{"~/projects/foo", false},
		{"C:\\Users\\scott\\projects\\foo", false},
		{"C:/Users/scott/projects/foo", false},

		// Bare directory name — should return false
		{"foo", false},

		// file:// URIs — explicit local git remotes are allowed
		{"file:///tmp/local-repo.git", true},
		{"file:///Users/scott/projects/foo", true},
		{"file://user@localhost:/tmp/local-repo.git", true},

		// Argument injection — should return false
		{"-oProxyCommand=evil", false},
		{"--upload-pack=touch /tmp/pwned", false},
		{"-c", false},

		// Malformed SCP-style — should return false
		{"@host:path", false},     // empty user
		{"user@:/path", false},    // empty host
		{"localhost:path", false}, // no user (not SCP-style)
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isGitRemoteURL(tt.input)
			if got != tt.want {
				t.Errorf("isGitRemoteURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// townConfigGitRecorder records what commitTownConfigChanges stages and commits.
type townConfigGitRecorder struct {
	added     []string
	msg       string
	committed []string
}

func (g *townConfigGitRecorder) Add(paths ...string) error {
	g.added = append(g.added, paths...)
	return nil
}

func (g *townConfigGitRecorder) CommitPaths(message string, paths ...string) error {
	g.msg = message
	g.committed = append([]string(nil), paths...)
	return nil
}

// TestCommitTownConfigChangesNamesOnlyItsPaths: the town repo is shared, so the
// config commit must name the four town files it owns rather than committing
// whatever else is staged in that repo (gt-2czgm).
func TestCommitTownConfigChangesNamesOnlyItsPaths(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, p := range []string{"mayor/rigs.json", "mayor/town.json", ".beads/routes.jsonl"} {
		path := filepath.Join(townRoot, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// mayor/daemon.json is intentionally absent: adopt need not create it.

	rec := &townConfigGitRecorder{}

	commitTownConfigChangesWith(rec, townRoot, "gastown")

	want := []string{"mayor/rigs.json", "mayor/town.json", ".beads/routes.jsonl"}
	if !slices.Equal(rec.committed, want) {
		t.Errorf("commit named %v, want %v", rec.committed, want)
	}
	if !slices.Equal(rec.added, want) {
		t.Errorf("staged %v, want %v", rec.added, want)
	}
	if !strings.Contains(rec.msg, "gastown") {
		t.Errorf("commit message %q does not name the rig", rec.msg)
	}
}

// TestRigHelpNamesTheRegistrysRealHome: the rig commands an operator reads
// help for must name where the registry lives — the "registry" section of
// mayor/town.json on a two-file town — and not the retired mayor/rigs.json,
// which a two-file town does not have (gt-51bdr).
func TestRigHelpNamesTheRegistrysRealHome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"add", rigAddCmd},
		{"remove", rigRemoveCmd},
		{"park", rigParkCmd},
		{"unpark", rigUnparkCmd},
	} {
		help := strings.Join([]string{tc.cmd.Short, tc.cmd.Long, tc.cmd.Flags().FlagUsages()}, "\n")
		if strings.Contains(help, "mayor/rigs.json") {
			t.Errorf("gt rig %s help still names the retired mayor/rigs.json:\n%s", tc.name, help)
		}
		if !strings.Contains(help, "mayor/town.json") || !strings.Contains(help, "registry") {
			t.Errorf("gt rig %s help does not name the registry in mayor/town.json:\n%s", tc.name, help)
		}
	}
}

// TestMigrateParkedRigsNamesTheRegistryItWroteTo: the --migrate line names the
// file the records landed in, so a two-file town is told the registry section
// of mayor/town.json and never mayor/rigs.json, which is not there (gt-51bdr).
func TestMigrateParkedRigsNamesTheRegistryItWroteTo(t *testing.T) {
	t.Parallel()
	entry := `{"git_url":"x","added_at":"2026-09-07T00:00:00Z","beads":{"repo":"","prefix":"tr"}}`
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json":                 `{"type":"town","version":2,"name":"t","registry":{"version":1,"rigs":{"testrig":` + entry + `}}}`,
		".beads-wisp/config/testrig.json": `{"rig":"testrig","values":{"status":"parked"}}`,
	})
	var out bytes.Buffer
	if err := migrateParkedRigs(&out, town, "sloan"); err != nil {
		t.Fatalf("migrateParkedRigs = %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "testrig") {
		t.Fatalf("migrate output does not name the migrated rig: %q", got)
	}
	if strings.Contains(got, "mayor/rigs.json") {
		t.Errorf("migrate output names the retired mayor/rigs.json: %q", got)
	}
	if want := `mayor/town.json (section "registry")`; !strings.Contains(got, want) {
		t.Errorf("migrate output = %q, want it to name %q", got, want)
	}
}

// unavailableShowBeads is a beads.Client whose Show fails the way bd does when
// it cannot answer (a timeout, a crash), which is not the same as the bead
// being absent.
type unavailableShowBeads struct{ beads.Client }

func (unavailableShowBeads) Show(string) (*beads.Issue, error) {
	return nil, fmt.Errorf("%w: dolt timeout", beads.ErrUnavailable)
}

// TestUndockRig_ShowErrorIsNotNotDocked: when the identity bead cannot be read,
// undock must fail rather than report the rig as not docked and exit 0, which
// would leave status:docked in place.
func TestUndockRig_ShowErrorIsNotNotDocked(t *testing.T) {
	t.Parallel()
	err := undockRig(t.TempDir(), unavailableShowBeads{}, "gt-gastown", "gastown")
	if err == nil {
		t.Fatal("undockRig with unreadable identity bead = nil; want error")
	}
	if !errors.Is(err, beads.ErrUnavailable) {
		t.Errorf("undockRig error = %v; want it to wrap beads.ErrUnavailable", err)
	}
}

// TestUndockRig_ShowNotFoundIsNotDocked: a legacy rig with no identity bead is
// not docked, so undock reports that and succeeds.
func TestUndockRig_ShowNotFoundIsNotDocked(t *testing.T) {
	t.Parallel()
	if err := undockRig(t.TempDir(), notFoundBeads{}, "gt-gastown", "gastown"); err != nil {
		t.Errorf("undockRig with no identity bead = %v; want nil", err)
	}
}

// dockedRecordingBeads is a docked identity bead whose Update calls are
// recorded.
type dockedRecordingBeads struct {
	beads.Client
	updates []string
}

func (b *dockedRecordingBeads) Show(string) (*beads.Issue, error) {
	return &beads.Issue{Labels: []string{RigDockedLabel}}, nil
}

func (b *dockedRecordingBeads) Update(id string, _ beads.UpdateOptions) error {
	b.updates = append(b.updates, id)
	return nil
}

// TestUndockRig_StampsTheRig: undocking a docked rig stamps its dock stamp,
// which is what tells a running daemon that the memoized identity-bead read it
// holds is stale. Without it the daemon keeps a docked verdict for the rest of
// its window.
func TestUndockRig_StampsTheRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	bd := &dockedRecordingBeads{}

	if err := undockRig(townRoot, bd, "gt-gastown", "gastown"); err != nil {
		t.Fatalf("undockRig = %v", err)
	}
	if !slices.Equal(bd.updates, []string{"gt-gastown"}) {
		t.Fatalf("Update calls = %q; want the docked label removed", bd.updates)
	}
	stamp, err := rig.DockStamp(townRoot, "gastown")
	if err != nil {
		t.Fatalf("DockStamp = %v", err)
	}
	if stamp.IsZero() {
		t.Error("undocking a rig stamped no dock stamp; the daemon would not re-read its bead")
	}
}

// TestUndockRig_StampFailureIsReported: the label is removed, but a stamp that
// could not be written is reported rather than swallowed, because the daemon
// will not see the undock until it re-reads the bead.
func TestUndockRig_StampFailureIsReported(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A regular file where the rig's runtime directory belongs, so the stamp
	// cannot be created.
	rigRuntime := filepath.Dir(rig.DockStampPath(townRoot, "gastown"))
	if err := os.MkdirAll(filepath.Dir(rigRuntime), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigRuntime, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	bd := &dockedRecordingBeads{}

	if err := undockRig(townRoot, bd, "gt-gastown", "gastown"); err == nil {
		t.Fatal("undockRig with an unwritable dock stamp = nil; want the failure reported")
	}
	if !slices.Equal(bd.updates, []string{"gt-gastown"}) {
		t.Errorf("Update calls = %q; want the undock itself still written", bd.updates)
	}
}

// labelsBeads is a beads.Client whose Show returns an identity bead with the
// given labels.
type labelsBeads struct {
	beads.Client
	labels []string
}

func (b labelsBeads) Show(string) (*beads.Issue, error) {
	return &beads.Issue{Labels: b.labels}, nil
}

// TestRigBeadDocked: only an identity bead carrying the docked label is docked,
// a missing one is not, and an unreadable one is an error rather than "not
// docked", which would let a docked rig start.
func TestRigBeadDocked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		bd      beads.Client
		want    bool
		wantErr error
	}{
		{"docked label", labelsBeads{labels: []string{"gt:rig", RigDockedLabel}}, true, nil},
		{"no docked label", labelsBeads{labels: []string{"gt:rig"}}, false, nil},
		{"no identity bead", notFoundBeads{}, false, nil},
		{"unreadable identity bead", unavailableShowBeads{}, false, beads.ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rigBeadDocked(tt.bd, "gt-gastown")
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("rigBeadDocked error = %v; want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("rigBeadDocked = %v; want %v", got, tt.want)
			}
		})
	}
}

// stubDockPolecatSessions is a dockPolecatSessions with canned answers that
// counts StopAll calls.
type stubDockPolecatSessions struct {
	infos    []polecat.SessionInfo
	listErr  error
	stopErr  error
	stopCall int
}

func (s *stubDockPolecatSessions) List() ([]polecat.SessionInfo, error) {
	return s.infos, s.listErr
}

func (s *stubDockPolecatSessions) StopAll(bool) error {
	s.stopCall++
	return s.stopErr
}

// TestStopDockedPolecats_ListErrorIsReturned: a polecat session list that cannot
// be read fails the dock. Skipping the stop block would write the docked label
// while the sessions still run.
func TestStopDockedPolecats_ListErrorIsReturned(t *testing.T) {
	t.Parallel()
	listErr := errors.New("tmux boom")
	stub := &stubDockPolecatSessions{listErr: listErr}
	stopped, err := stopDockedPolecats(stub)
	if err == nil {
		t.Fatal("stopDockedPolecats with unreadable session list = nil; want error")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("stopDockedPolecats error = %v; want it to wrap %v", err, listErr)
	}
	if stub.stopCall != 0 {
		t.Errorf("StopAll calls = %d; want 0", stub.stopCall)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %q; want none", stopped)
	}
}

// TestStopDockedPolecats: with a readable session list, docking stops the polecat
// sessions it found and reports them. No sessions stops nothing, and a failed
// stop is an error: the sessions still run.
func TestStopDockedPolecats(t *testing.T) {
	t.Parallel()
	infos := []polecat.SessionInfo{{Polecat: "furiosa", Running: true}}
	stopErr := errors.New("kill failed")
	tests := []struct {
		name     string
		stub     stubDockPolecatSessions
		wantStop int
		wantOut  []string
		wantErr  error
	}{
		{"no sessions", stubDockPolecatSessions{}, 0, nil, nil},
		{"stopped", stubDockPolecatSessions{infos: infos}, 1, []string{"1 polecat session(s) stopped"}, nil},
		{"stop error", stubDockPolecatSessions{infos: infos, stopErr: stopErr}, 1, nil, stopErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stub := tt.stub // a copy: the subtests run in parallel
			stopped, err := stopDockedPolecats(&stub)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("stopDockedPolecats error = %v; want %v", err, tt.wantErr)
			}
			if !slices.Equal(stopped, tt.wantOut) {
				t.Errorf("stopped = %q; want %q", stopped, tt.wantOut)
			}
			if stub.stopCall != tt.wantStop {
				t.Errorf("StopAll calls = %d; want %d", stub.stopCall, tt.wantStop)
			}
		})
	}
}

// recordingUpdateBeads is a beads.Client that records the Update calls it gets.
type recordingUpdateBeads struct {
	beads.Client
	updateErr error
	updates   []string
}

func (b *recordingUpdateBeads) Update(id string, _ beads.UpdateOptions) error {
	b.updates = append(b.updates, id)
	return b.updateErr
}

// TestStopAndDockRig: the docked label is written only once the polecat
// sessions are stopped. An unreadable session list or a failed stop leaves the
// identity bead untouched, so the rig is not docked while its sessions run.
func TestStopAndDockRig(t *testing.T) {
	t.Parallel()
	infos := []polecat.SessionInfo{{Polecat: "furiosa", Running: true}}
	listErr := errors.New("tmux boom")
	stopErr := errors.New("kill failed")
	updateErr := errors.New("dolt timeout")
	tests := []struct {
		name        string
		stub        stubDockPolecatSessions
		updateErr   error
		wantErr     error
		wantUpdates []string
		wantOut     []string
	}{
		{"stopped", stubDockPolecatSessions{infos: infos}, nil, nil, []string{"gt-gastown"}, []string{"1 polecat session(s) stopped"}},
		{"no sessions", stubDockPolecatSessions{}, nil, nil, []string{"gt-gastown"}, nil},
		{"list error", stubDockPolecatSessions{listErr: listErr}, nil, listErr, nil, nil},
		{"stop error", stubDockPolecatSessions{infos: infos, stopErr: stopErr}, nil, stopErr, nil, nil},
		{"label error", stubDockPolecatSessions{infos: infos}, updateErr, updateErr, []string{"gt-gastown"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stub := tt.stub // a copy: the subtests run in parallel
			townRoot := t.TempDir()
			bd := &recordingUpdateBeads{updateErr: tt.updateErr}
			stopped, err := stopAndDockRig(townRoot, "gastown", bd, "gt-gastown", &stub)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("stopAndDockRig error = %v; want %v", err, tt.wantErr)
			}
			if !slices.Equal(bd.updates, tt.wantUpdates) {
				t.Errorf("Update calls = %q; want %q", bd.updates, tt.wantUpdates)
			}
			if !slices.Equal(stopped, tt.wantOut) {
				t.Errorf("stopped = %q; want %q", stopped, tt.wantOut)
			}
			// A dock that landed stamps the rig, which is the only signal a
			// running daemon has that its memoized read is stale. A dock whose
			// label write failed did not happen, so it must not stamp.
			stamp, stampErr := rig.DockStamp(townRoot, "gastown")
			if stampErr != nil {
				t.Fatalf("DockStamp = %v", stampErr)
			}
			landed := len(tt.wantUpdates) > 0 && tt.updateErr == nil
			if landed != !stamp.IsZero() {
				t.Errorf("dock stamp present = %v after updates %q; want %v",
					!stamp.IsZero(), tt.wantUpdates, landed)
			}
		})
	}
}

// TestStopAndDockRig_StampFailureIsReported: the docked label is written, but a
// dock stamp that could not be written is reported rather than swallowed, because
// a running daemon will not see the dock until it re-reads the bead.
func TestStopAndDockRig_StampFailureIsReported(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A regular file where the rig's runtime directory belongs, so the stamp
	// cannot be created.
	rigRuntime := filepath.Dir(rig.DockStampPath(townRoot, "gastown"))
	if err := os.MkdirAll(filepath.Dir(rigRuntime), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigRuntime, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	bd := &recordingUpdateBeads{}
	stub := &stubDockPolecatSessions{}

	if _, err := stopAndDockRig(townRoot, "gastown", bd, "gt-gastown", stub); err == nil {
		t.Fatal("stopAndDockRig with an unwritable dock stamp = nil; want the failure reported")
	}
	if !slices.Equal(bd.updates, []string{"gt-gastown"}) {
		t.Errorf("Update calls = %q; want the dock itself still written", bd.updates)
	}
}
