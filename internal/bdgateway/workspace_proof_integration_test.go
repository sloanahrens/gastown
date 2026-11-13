//go:build integration

// The Phase 0 proof of docs/design/bd-container.md: a real bd reads a bead
// through a workspace tree laid out the way the gateway's PUT /v1/workspace
// will push one, with the redirect and the Dolt endpoint rewritten into that
// tree. It skips unless BD_PROOF_BEAD names a bead and the Dolt endpoint is
// set, so a sandbox starts no bd and reaches no network; the env and bd-result
// tests start child copies of this test binary and run it against commands
// standing in for bd.
//
// Running it. The proof needs a real bd and a Dolt endpoint that holds a bead;
// elsewhere it skips, unless BD_PROOF_REQUIRE says a missing prerequisite must
// fail instead. The overseer sets:
//
//	BD_PROOF_BEAD                a bead id in the endpoint's database, e.g. gt-p02maf
//	BEADS_DOLT_SERVER_HOST       the Dolt endpoint bd reads
//	BEADS_DOLT_SERVER_PORT       the endpoint's port
//	BD_REAL_BIN                  optional: the bd to run; PATH's bd otherwise
//	BD_PROOF_PROJECT_ID          optional: a uuid written into metadata.json, which
//	                             exercises bd's project-id check
//	BD_PROOF_REQUIRE=1           a missing prerequisite fails instead of skipping
//
// and runs, from the repository root:
//
//	go vet -tags integration ./internal/bdgateway
//	BD_PROOF_BEAD=gt-p02maf BEADS_DOLT_SERVER_HOST=<host> \
//	  BEADS_DOLT_SERVER_PORT=<port> BD_PROOF_REQUIRE=1 \
//	  go test -tags integration ./internal/bdgateway -count=1 -v
//
// The integration run takes no -run filter — the build tag selects the tier and
// that tags every test in this package (docs/testing.md) — so a filter that
// matches no name cannot turn the proof into a vacuous pass.

package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// proofTimeout bounds the one bd process the proof starts. bd's first call of
// a process costs under a second on a warm host (the design's latency budget),
// so the proof fails rather than hangs a gate.
const proofTimeout = 2 * time.Minute

// The environments that decide whether the proof runs. BD_PROOF_BEAD names a
// bead the Dolt server already holds; the two BEADS_DOLT_SERVER_* names are
// bd's own endpoint overrides.
const (
	proofBeadEnv     = "BD_PROOF_BEAD"
	proofProjectEnv  = "BD_PROOF_PROJECT_ID"
	proofDoltHostEnv = "BEADS_DOLT_SERVER_HOST"
	proofDoltPortEnv = "BEADS_DOLT_SERVER_PORT"
	proofBdBinEnv    = "BD_REAL_BIN"
	proofRequireEnv  = "BD_PROOF_REQUIRE"
)

// The reasons a sandbox run skips instead of failing.
const (
	skipNoBead     = proofBeadEnv + " is unset: the proof has no bead to read"
	skipNoEndpoint = proofDoltHostEnv + " and " + proofDoltPortEnv + " must both name the Dolt endpoint"
	skipNoBd       = "bd is not on PATH and " + proofBdBinEnv + " is unset"
)

// proofSetup is what the proof resolved from the environment.
type proofSetup struct {
	bdPath    string
	beadID    string
	rig       string // the beads directory name and Dolt database, the bead id's prefix
	prefix    string // the bead id's prefix with its trailing dash
	host      string
	port      string
	projectID string // optional: the Dolt database's project id, kept when set
}

// proofTree is the temp workspace tree writeProofWorkspace laid out.
type proofTree struct {
	root     string // the stand-in for the gateway's /ws
	beadsDir string // the rig's redirect directory, the BEADS_DIR of the run
}

// proofRequired reads BD_PROOF_REQUIRE. An unrecognized value is an error, not
// off: a guard that cannot read its switch must not pass as clean.
func proofRequired(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no":
		return false, nil
	case "1", "true", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("%s %q is not a boolean: set 1 to fail when a prerequisite is missing, or leave it unset to skip", proofRequireEnv, raw)
	}
}

// proofPrereqs resolves the real bd, the Dolt endpoint and the proof bead. A
// non-empty skip means the environment cannot run the proof; an error means it
// is misconfigured and the proof must not pass silently. With
// BD_PROOF_REQUIRE set, a missing prerequisite is an error rather than a skip,
// so a gate command can demand that the proof really ran.
func proofPrereqs(getenv func(string) string, lookPath func(string) (string, error)) (setup proofSetup, skip string, err error) {
	required, err := proofRequired(getenv(proofRequireEnv))
	if err != nil {
		return proofSetup{}, "", err
	}
	unavailable := func(reason string) (proofSetup, string, error) {
		if required {
			return proofSetup{}, "", fmt.Errorf("%s is set but the proof cannot run: %s", proofRequireEnv, reason)
		}
		return proofSetup{}, reason, nil
	}
	beadID := strings.TrimSpace(getenv(proofBeadEnv))
	if beadID == "" {
		return unavailable(skipNoBead)
	}
	prefix, _, ok := strings.Cut(beadID, "-")
	if !ok || prefix == "" {
		return proofSetup{}, "", fmt.Errorf("%s %q has no prefix before a dash, so the proof cannot name the gateway workspace", proofBeadEnv, beadID)
	}
	// The prefix becomes a directory name under the temp root, so it must be one
	// path component: a bead id of "../evil-1" would otherwise write the tree
	// outside the tree.
	if prefix == "." || prefix == ".." || strings.ContainsAny(prefix, `/\`) {
		return proofSetup{}, "", fmt.Errorf("%s %q names a prefix that is not a single path component", proofBeadEnv, beadID)
	}
	host := strings.TrimSpace(getenv(proofDoltHostEnv))
	port := strings.TrimSpace(getenv(proofDoltPortEnv))
	if host == "" || port == "" {
		return unavailable(skipNoEndpoint)
	}
	bdPath := strings.TrimSpace(getenv(proofBdBinEnv))
	if bdPath == "" {
		bdPath, err = lookPath("bd")
		if err != nil {
			return unavailable(skipNoBd)
		}
	}
	return proofSetup{
		bdPath:    bdPath,
		beadID:    beadID,
		rig:       prefix,
		prefix:    prefix + "-",
		host:      host,
		port:      port,
		projectID: strings.TrimSpace(getenv(proofProjectEnv)),
	}, "", nil
}

// writeProofWorkspace lays out the tree the gateway's PUT /v1/workspace will
// push: the town's routes.jsonl, the rig's redirect, and the rig's workspace
// files, with every absolute path pointing into root rather than the host.
func writeProofWorkspace(t *testing.T, root string, s proofSetup) proofTree {
	t.Helper()
	rigDir := filepath.Join(root, s.rig)
	beadsDir := filepath.Join(rigDir, ".beads")
	workspace := filepath.Join(rigDir, "mayor", "rig", ".beads")
	townBeads := filepath.Join(root, ".beads")
	for _, dir := range []string{townBeads, beadsDir, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	metadata, err := json.Marshal(struct {
		Database       string `json:"database"`
		Backend        string `json:"backend"`
		DoltMode       string `json:"dolt_mode"`
		DoltDatabase   string `json:"dolt_database"`
		DoltServerHost string `json:"dolt_server_host"`
		ProjectID      string `json:"project_id,omitempty"`
	}{
		Database:       "dolt",
		Backend:        "dolt",
		DoltMode:       "server",
		DoltDatabase:   s.rig,
		DoltServerHost: s.host,
		ProjectID:      s.projectID,
	})
	if err != nil {
		t.Fatalf("encoding metadata.json: %v", err)
	}

	routes := []struct {
		Path   string `json:"path"`
		Prefix string `json:"prefix"`
	}{
		{Path: ".", Prefix: "hq-"},
		{Path: filepath.ToSlash(filepath.Join(s.rig, "mayor", "rig")), Prefix: s.prefix},
	}
	var routeLines strings.Builder
	for _, route := range routes {
		line, err := json.Marshal(route)
		if err != nil {
			t.Fatalf("encoding routes.jsonl: %v", err)
		}
		routeLines.Write(line)
		routeLines.WriteByte('\n')
	}

	write(filepath.Join(townBeads, "routes.jsonl"), routeLines.String())
	write(filepath.Join(beadsDir, "redirect"), workspace+"\n")
	write(filepath.Join(workspace, "metadata.json"), string(metadata)+"\n")
	write(filepath.Join(workspace, "config.yaml"), fmt.Sprintf("issue-prefix: %q\n", s.prefix))
	write(filepath.Join(workspace, "dolt-server.port"), s.port)
	return proofTree{root: root, beadsDir: beadsDir}
}

// TestIntegrationWorkspaceProof is the Phase 0 proof: the gateway owns a temp
// workspace tree, and a real bd reads BD_PROOF_BEAD through it. The overseer
// runs it on the host with BD_PROOF_BEAD set to a bead in the endpoint's Dolt
// server; anywhere else it skips.
func TestIntegrationWorkspaceProof(t *testing.T) {
	setup, skip, err := proofPrereqs(os.Getenv, exec.LookPath)
	if err != nil {
		t.Fatal(err)
	}
	if skip != "" {
		t.Skip(skip)
	}
	tree := writeProofWorkspace(t, t.TempDir(), setup)

	ctx, cancel := context.WithTimeout(t.Context(), proofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, setup.bdPath, "show", setup.beadID)
	cmd.Dir = tree.root
	cmd.Env = append(os.Environ(),
		"BEADS_DIR="+tree.beadsDir,
		proofDoltHostEnv+"="+setup.host,
		proofDoltPortEnv+"="+setup.port,
	)
	out, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("bd show %s did not finish within %s", setup.beadID, proofTimeout)
	}
	if runErr != nil {
		t.Fatalf("bd show %s against the gateway workspace failed: %v\nbd output:\n%s", setup.beadID, runErr, out)
	}
	if !strings.Contains(string(out), setup.beadID) {
		t.Fatalf("bd show %s did not name the bead in its output:\n%s", setup.beadID, out)
	}
}

// runProofChild runs TestIntegrationWorkspaceProof in a child test binary with
// only env, and returns its combined output. Each case builds its whole
// environment, so the proof's real os.Getenv and exec.LookPath wiring decides
// the branch.
func runProofChild(t *testing.T, env []string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), proofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIntegrationWorkspaceProof$", "-test.v")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the child proof did not finish within %s:\n%s", proofTimeout, out)
	}
	return out, err
}

// TestIntegrationWorkspaceProofEnvBranches drives the proof's own skip and
// failure branches: each case re-runs TestIntegrationWorkspaceProof in a child
// process with only the listed environment, so it exercises the real os.Getenv
// and exec.LookPath wiring rather than a copy of it.
func TestIntegrationWorkspaceProofEnvBranches(t *testing.T) {
	const noPath = "PATH=/nonexistent"
	const requireOn = proofRequireEnv + "=1"
	cases := []struct {
		name     string
		env      []string
		wantText string
		wantFail bool
	}{
		{
			name:     "no bead",
			env:      []string{proofDoltHostEnv + "=dolt.test", proofDoltPortEnv + "=3307", noPath},
			wantText: skipNoBead,
		},
		{
			name:     "no endpoint",
			env:      []string{proofBeadEnv + "=gt-probe-1", proofDoltHostEnv + "=dolt.test", noPath},
			wantText: skipNoEndpoint,
		},
		{
			name:     "no bd",
			env:      []string{proofBeadEnv + "=gt-probe-1", proofDoltHostEnv + "=dolt.test", proofDoltPortEnv + "=3307", noPath},
			wantText: skipNoBd,
		},
		{
			name:     "bead id without a prefix",
			env:      []string{proofBeadEnv + "=probe1", proofDoltHostEnv + "=dolt.test", proofDoltPortEnv + "=3307", noPath},
			wantText: "has no prefix before a dash",
			wantFail: true,
		},
		{
			name:     "bead id prefix escapes the temp tree",
			env:      []string{proofBeadEnv + "=../evil-1", proofDoltHostEnv + "=dolt.test", proofDoltPortEnv + "=3307", noPath},
			wantText: "is not a single path component",
			wantFail: true,
		},
		{
			name:     "required run with no bead",
			env:      []string{requireOn, proofDoltHostEnv + "=dolt.test", proofDoltPortEnv + "=3307", noPath},
			wantText: proofRequireEnv + " is set but the proof cannot run: " + skipNoBead,
			wantFail: true,
		},
		{
			name:     "required run with no bd",
			env:      []string{requireOn, proofBeadEnv + "=gt-probe-1", proofDoltHostEnv + "=dolt.test", proofDoltPortEnv + "=3307", noPath},
			wantText: proofRequireEnv + " is set but the proof cannot run: " + skipNoBd,
			wantFail: true,
		},
		{
			name:     "required run with an unreadable value",
			env:      []string{proofRequireEnv + "=maybe", noPath},
			wantText: "is not a boolean",
			wantFail: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runProofChild(t, tc.env)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("the child proof exited 0, want a failure:\n%s", out)
				}
			} else if err != nil {
				t.Fatalf("the child proof exited %v, want a skip:\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.wantText) {
				t.Fatalf("the child proof's output does not contain %q:\n%s", tc.wantText, out)
			}
		})
	}
}

// TestIntegrationWorkspaceProofBdResult drives the proof's verdict on what bd
// returns: a command naming the bead passes, one exiting nonzero fails, and one
// exiting 0 without the bead id fails. Coreutils commands stand in for bd, so
// the proof runs its real exec and assertions without a Dolt endpoint.
func TestIntegrationWorkspaceProofBdResult(t *testing.T) {
	cases := []struct {
		name     string
		command  string // the command the proof runs in bd's place
		wantFail bool
		wantText string
	}{
		{name: "bd names the bead", command: "echo", wantText: "--- PASS: TestIntegrationWorkspaceProof"},
		// ls fails on the two path arguments and names the bead in its error, so
		// only the exit-status check can reject this run.
		{name: "bd exits nonzero", command: "ls", wantFail: true, wantText: "against the gateway workspace failed"},
		{name: "bd omits the bead id", command: "true", wantFail: true, wantText: "did not name the bead"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, lookErr := exec.LookPath(tc.command)
			if lookErr != nil {
				t.Fatalf("%s is not on PATH, so it cannot stand in for bd: %v", tc.command, lookErr)
			}
			out, err := runProofChild(t, []string{
				proofRequireEnv + "=1",
				proofBeadEnv + "=gt-probe-1",
				proofDoltHostEnv + "=dolt.gateway.test",
				proofDoltPortEnv + "=3307",
				proofBdBinEnv + "=" + bin,
				"PATH=/nonexistent",
			})
			if tc.wantFail {
				if err == nil {
					t.Fatalf("the child proof exited 0, want a failure:\n%s", out)
				}
			} else if err != nil {
				t.Fatalf("the child proof exited %v, want a pass:\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.wantText) {
				t.Fatalf("the child proof's output does not contain %q:\n%s", tc.wantText, out)
			}
		})
	}
}

// TestIntegrationWorkspaceProofLayout checks the tree the proof runs against:
// the five files G7 will push, with the redirect and metadata pointing into the
// tree rather than the host. It needs no bd and no endpoint.
func TestIntegrationWorkspaceProofLayout(t *testing.T) {
	setup := proofSetup{
		beadID: "gt-probe-1",
		rig:    "gt",
		prefix: "gt-",
		host:   "dolt.gateway.test",
		port:   "3307",
	}
	tree := writeProofWorkspace(t, t.TempDir(), setup)
	workspace := filepath.Join(tree.root, "gt", "mayor", "rig", ".beads")

	redirect, err := os.ReadFile(filepath.Join(tree.beadsDir, "redirect"))
	if err != nil {
		t.Fatalf("reading redirect: %v", err)
	}
	if want := workspace + "\n"; string(redirect) != want {
		t.Errorf("redirect = %q, want %q", redirect, want)
	}
	if !filepath.IsAbs(strings.TrimSpace(string(redirect))) {
		t.Errorf("redirect %q is not absolute; the gateway rewrites it into its own root", redirect)
	}

	metadata, err := os.ReadFile(filepath.Join(workspace, "metadata.json"))
	if err != nil {
		t.Fatalf("reading metadata.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("parsing metadata.json: %v", err)
	}
	wantFields := map[string]string{
		"database":         "dolt",
		"backend":          "dolt",
		"dolt_mode":        "server",
		"dolt_database":    "gt",
		"dolt_server_host": "dolt.gateway.test",
	}
	for key, want := range wantFields {
		if meta[key] != want {
			t.Errorf("metadata.json %s = %v, want %q", key, meta[key], want)
		}
	}
	if _, ok := meta["project_id"]; ok {
		t.Errorf("metadata.json names a project_id the temp tree was never given: %v", meta["project_id"])
	}

	port, err := os.ReadFile(filepath.Join(workspace, "dolt-server.port"))
	if err != nil {
		t.Fatalf("reading dolt-server.port: %v", err)
	}
	if string(port) != "3307" {
		t.Errorf("dolt-server.port = %q, want %q", port, "3307")
	}

	config, err := os.ReadFile(filepath.Join(workspace, "config.yaml"))
	if err != nil {
		t.Fatalf("reading config.yaml: %v", err)
	}
	if !strings.Contains(string(config), `issue-prefix: "gt-"`) {
		t.Errorf("config.yaml does not carry the workspace prefix:\n%s", config)
	}

	routes, err := os.ReadFile(filepath.Join(tree.root, ".beads", "routes.jsonl"))
	if err != nil {
		t.Fatalf("reading routes.jsonl: %v", err)
	}
	for _, want := range []string{`{"path":".","prefix":"hq-"}`, `{"path":"gt/mayor/rig","prefix":"gt-"}`} {
		if !strings.Contains(string(routes), want) {
			t.Errorf("routes.jsonl does not contain %s:\n%s", want, routes)
		}
	}
}

// TestIntegrationWorkspaceProofLayoutProjectID checks that a project id given
// to the proof reaches metadata.json, because bd refuses a workspace whose
// project id differs from the database's.
func TestIntegrationWorkspaceProofLayoutProjectID(t *testing.T) {
	const id = "8f550f3e-d9e1-452f-bb70-6a991fdb8fdb"
	setup := proofSetup{
		beadID:    "hq-probe-1",
		rig:       "hq",
		prefix:    "hq-",
		host:      "dolt.gateway.test",
		port:      "3307",
		projectID: id,
	}
	tree := writeProofWorkspace(t, t.TempDir(), setup)
	metadata, err := os.ReadFile(filepath.Join(tree.root, "hq", "mayor", "rig", ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("reading metadata.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("parsing metadata.json: %v", err)
	}
	if meta["project_id"] != id {
		t.Errorf("metadata.json project_id = %v, want %q", meta["project_id"], id)
	}
}
