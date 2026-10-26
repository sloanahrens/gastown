//go:build integration

package formula

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The gate tier holds the dispatch guards' shape (TestPRFeedbackPatrolGuards-
// ReadBeadsFromJSON). These tests hold their behaviour: they run the patrol's
// own shell blocks with a stub bd first on PATH, so a populated, empty and
// failing read each show what the guards actually do (gt-497s7).

// patrolStubBd answers the guard's `bd list --json` call: an empty array, a
// populated one (BD_BEADS entries), or a hard failure. It logs every call so a
// test can see which flags the guard asked for.
const patrolStubBd = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$BD_LOG"
if [ "${BD_MODE:-none}" = "fail" ]; then
  echo "dolt connection refused" >&2
  exit 2
fi
printf '['
i=1
while [ "$i" -le "${BD_BEADS:-0}" ]; do
  [ "$i" -gt 1 ] && printf ','
  printf '{"id":"bd-%s"}' "$i"
  i=$((i+1))
done
printf ']\n'
`

// patrolStubGt records the verbs the loop reaches and answers `bead create`
// with an id, so a test can tell a dispatched finding from a skipped one.
const patrolStubGt = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GT_LOG"
if [ "$1" = "bead" ] && [ "$2" = "create" ]; then
  echo "bd-new-1"
fi
exit 0
`

// patrolHarness runs a rendered guard block with stub bd and gt commands first
// on PATH.
type patrolHarness struct {
	dir      string
	bin      string
	bdLog    string
	gtLog    string
	findings string
}

func newPatrolHarness(t *testing.T) *patrolHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the guards are POSIX shell pipelines")
	}
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH; the guards count beads with it", tool)
		}
	}
	h := &patrolHarness{
		dir:      t.TempDir(),
		bin:      filepath.Join(t.TempDir(), "bin"),
		findings: filepath.Join(t.TempDir(), "patrol-findings.txt"),
	}
	if err := os.MkdirAll(h.bin, 0o755); err != nil {
		t.Fatalf("creating stub bin: %v", err)
	}
	h.bdLog = filepath.Join(h.dir, "bd.log")
	h.gtLog = filepath.Join(h.dir, "gt.log")
	writeExecutable(t, filepath.Join(h.bin, "bd"), patrolStubBd)
	writeExecutable(t, filepath.Join(h.bin, "gt"), patrolStubGt)
	return h
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// run executes a block in the harness; BD_MODE and BD_BEADS are read by the
// stub bd.
func (h *patrolHarness) run(t *testing.T, block string, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	script := filepath.Join(h.dir, "guard.sh")
	if err := os.WriteFile(script, []byte(block), 0o644); err != nil {
		t.Fatalf("writing guard script: %v", err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = h.dir
	env := []string{
		"PATH=" + h.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BD_LOG=" + h.bdLog,
		"GT_LOG=" + h.gtLog,
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PATH=") || strings.HasPrefix(e, "BD_LOG=") || strings.HasPrefix(e, "GT_LOG=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = append(env, extra...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func (h *patrolHarness) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// TestIntegrationPRFeedbackPatrolGuardsFailClosed holds the two guards' rules
// under every read bd can give them: the valve compares the open patrol-bead
// count against max_open_beads, the dedup guard dispatches a finding only when
// no bead is open for its PR, and neither treats a failed read as zero.
func TestIntegrationPRFeedbackPatrolGuardsFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("valve", func(t *testing.T) {
		t.Parallel()
		valve, _ := patrolDispatchBlocks(t)
		valve += "\necho dispatch-continued\n"
		cases := []struct {
			name         string
			mode         string
			beads        int
			wantDispatch bool
			wantValve    bool
		}{
			{name: "no open beads dispatches", beads: 0, wantDispatch: true},
			{name: "below max dispatches", beads: patrolTestMaxOpen - 1, wantDispatch: true},
			{name: "at max trips the valve", beads: patrolTestMaxOpen, wantValve: true},
			{name: "over max trips the valve", beads: patrolTestMaxOpen + 3, wantValve: true},
			{name: "bd failure stops dispatch", mode: "fail"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newPatrolHarness(t)
				stdout, stderr, err := h.run(t, valve,
					fmt.Sprintf("BD_MODE=%s", tc.mode),
					fmt.Sprintf("BD_BEADS=%d", tc.beads))

				if got := strings.Contains(stdout, "dispatch-continued"); got != tc.wantDispatch {
					t.Errorf("dispatch continued = %v, want %v (stdout %q, stderr %q, err %v)", got, tc.wantDispatch, stdout, stderr, err)
				}
				if got := strings.Contains(stdout, "Safety valve"); got != tc.wantValve {
					t.Errorf("safety valve tripped = %v, want %v (stdout %q)", got, tc.wantValve, stdout)
				}
				if tc.mode == "fail" {
					if err == nil {
						t.Error("a bd list failure exited 0; the valve read it as zero open beads")
					}
					if !strings.Contains(stderr, "bd list failed") {
						t.Errorf("a bd list failure did not say so on stderr: %q", stderr)
					}
				} else if err != nil {
					t.Errorf("block exited with %v; stderr %q", err, stderr)
				}
				assertUnlimitedJSONRead(t, h.bdLog)
			})
		}
	})

	t.Run("dedup", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name       string
			mode       string
			beads      int
			wantCreate bool
			wantSkip   bool
		}{
			{name: "no open bead dispatches", beads: 0, wantCreate: true},
			{name: "open bead skips the PR", beads: 1, wantSkip: true},
			{name: "bd failure skips the PR", mode: "fail", wantSkip: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newPatrolHarness(t)
				if err := os.WriteFile(h.findings, []byte("review-feedback:7:https://example.test/pr/7:Add thing:\n"), 0o644); err != nil {
					t.Fatalf("writing findings: %v", err)
				}
				// The findings file is production state the block shares with a
				// running patrol; the test points it at its own temp file.
				_, loop := patrolDispatchBlocks(t)
				loop = strings.ReplaceAll(loop, "/tmp/patrol-findings.txt", h.findings)

				stdout, stderr, err := h.run(t, loop,
					fmt.Sprintf("BD_MODE=%s", tc.mode),
					fmt.Sprintf("BD_BEADS=%d", tc.beads))
				if err != nil {
					t.Errorf("block exited with %v; stderr %q", err, stderr)
				}

				if got := strings.Contains(h.read(t, h.gtLog), "bead create"); got != tc.wantCreate {
					t.Errorf("created a bead = %v, want %v (gt calls %q, stdout %q)", got, tc.wantCreate, h.read(t, h.gtLog), stdout)
				}
				skipped := strings.Contains(stdout, "open bead already exists") || strings.Contains(stderr, "bd list failed")
				if skipped != tc.wantSkip {
					t.Errorf("skipped the PR = %v, want %v (stdout %q, stderr %q)", skipped, tc.wantSkip, stdout, stderr)
				}
				if tc.mode == "fail" && !strings.Contains(stderr, "skipping PR #7") {
					t.Errorf("a bd list failure did not name the PR it skipped: %q", stderr)
				}
				assertUnlimitedJSONRead(t, h.bdLog)
			})
		}
	})
}

// assertUnlimitedJSONRead checks the guard asked bd for the JSON array and for
// no row cap: `bd list` defaults to 50 rows, so a capped read undercounts a
// large backlog and the valve never trips (`bd list --help`).
func assertUnlimitedJSONRead(t *testing.T, bdLog string) {
	t.Helper()
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("stub bd was never called: %v", err)
	}
	for _, call := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		for _, want := range []string{"list", "--json", "--limit 0"} {
			if !strings.Contains(call, want) {
				t.Errorf("guard called bd without %q: %s", want, call)
			}
		}
	}
}
