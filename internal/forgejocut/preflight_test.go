package forgejocut

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// fakeRunner is the Runner seam: it records each argv in order and answers every
// call with the stdout and error it is given.
type fakeRunner struct {
	out   []byte
	err   error
	calls [][]string
}

func (f *fakeRunner) Output(argv []string) ([]byte, error) {
	f.calls = append(f.calls, argv)
	return f.out, f.err
}

func (f *fakeRunner) Run(argv []string) error {
	f.calls = append(f.calls, argv)
	return f.err
}

// errExit stands in for the exit status of a failed child process.
var errExit = errors.New("exit status 1")

// queueRoot is the rig root the landing-queue read is run in.
const queueRoot = "/srv/town/gastown"

// queueArgv is the bd count the landing queue read runs.
var queueArgv = []string{"bd", "-C", queueRoot, "count", "--status", "open", "--label", land.LabelReadyToLand}

func TestLandingCountTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		err     error
		want    int
		wantErr bool
	}{
		{name: "zero", out: "0\n", want: 0},
		{name: "positive", out: "3\n", want: 3},
		{name: "no trailing newline", out: "12", want: 12},
		{name: "empty output", out: "", wantErr: true},
		{name: "non-numeric output", out: "three\n", wantErr: true},
		{name: "signed output", out: "-1\n", wantErr: true},
		{name: "count too large for an int", out: "99999999999999999999\n", wantErr: true},
		{name: "space around the count", out: " 2\n", wantErr: true},
		{name: "command failure", err: errExit, wantErr: true},
		{name: "failure despite a count", out: "2\n", err: errExit, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{out: []byte(tt.out), err: tt.err}
			got, err := LandingCount(run, queueRoot)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LandingCount(%q, err %v) error = %v, want error %v", tt.out, tt.err, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("LandingCount(%q, err %v) = %d, want %d", tt.out, tt.err, got, tt.want)
			}
			if len(run.calls) != 1 || !slices.Equal(run.calls[0], queueArgv) {
				t.Fatalf("LandingCount ran %q, want one call of %q", run.calls, queueArgv)
			}
		})
	}
}

func TestLandingCountNeverEchoesOutput(t *testing.T) {
	t.Parallel()
	const answer = "ghp_landingqueue_answer"
	run := &fakeRunner{out: []byte(answer + "\n")}
	_, err := LandingCount(run, queueRoot)
	if err == nil {
		t.Fatalf("LandingCount accepted the non-numeric answer %q", answer)
	}
	if strings.Contains(err.Error(), answer) {
		t.Fatalf("error %q echoes the command output", err)
	}
}

func TestLabelComesFromLand(t *testing.T) {
	t.Parallel()
	run := &fakeRunner{out: []byte("0\n")}
	if _, err := LandingCount(run, queueRoot); err != nil {
		t.Fatalf("LandingCount: %v", err)
	}
	i := slices.Index(run.calls[0], "--label")
	if i < 0 || i+1 >= len(run.calls[0]) || run.calls[0][i+1] != land.LabelReadyToLand {
		t.Fatalf("bd argv %q does not count the label land.LabelReadyToLand (%q)", run.calls[0], land.LabelReadyToLand)
	}
}

func TestRefuseIfLandingInFlightMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		err     error
		wantErr string
		wantLog []string
	}{
		{
			name:    "one bead in flight",
			out:     "1\n",
			wantErr: "gastown has 1 open gt:ready-to-land bead(s): land them (or wait) before cutting the rig over",
		},
		{
			name:    "several beads in flight",
			out:     "4\n",
			wantErr: "gastown has 4 open gt:ready-to-land bead(s): land them (or wait) before cutting the rig over",
		},
		{
			name:    "queue read fails",
			err:     errExit,
			wantErr: "could not read the landing queue of gastown: bd -C /srv/town/gastown count --status open --label gt:ready-to-land failed: exit status 1",
		},
		{
			name:    "queue answer is not a count",
			out:     "many\n",
			wantErr: "could not read the landing queue of gastown: bd -C /srv/town/gastown count --status open --label gt:ready-to-land printed no plain count",
		},
		{
			name:    "queue empty",
			out:     "0\n",
			wantLog: []string{"gastown's landing queue is empty"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := &Options{Rig: "gastown", RigRoot: queueRoot}
			run := &fakeRunner{out: []byte(tt.out), err: tt.err}
			var logs []string
			err := RefuseIfLandingInFlight(run, o, func(s string) { logs = append(logs, s) })
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("RefuseIfLandingInFlight = %v, want nil", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("RefuseIfLandingInFlight = %v, want %q", err, tt.wantErr)
			}
			if !slices.Equal(logs, tt.wantLog) {
				t.Fatalf("logs = %q, want %q", logs, tt.wantLog)
			}
		})
	}
}

// probe fixtures: the values a probe run is given, as the cutover derives them.
const (
	probeScript    = "/repo/scripts/forgejo-probe.sh"
	probeAPIURL    = "http://127.0.0.1:3000/api/v1"
	probeWebURL    = "http://127.0.0.1:3000"
	probeGitHub    = "https://github.com/acme/gastown.git"
	probeAdminGit  = "http://127.0.0.1:3000/acme/gastown.git"
	probeTokenFile = "/run/forgejo/admin.env"
)

// probeOptions is a rig whose probe run is fully derived, with no admin token file.
func probeOptions() *Options {
	return &Options{
		Rig:         "gastown",
		Repo:        "acme/gastown",
		MainBranch:  "main",
		TownRoot:    "/srv/town",
		APIURL:      probeAPIURL,
		WebURL:      probeWebURL,
		Probe:       probeScript,
		GitHubURL:   probeGitHub,
		AdminGitURL: probeAdminGit,
	}
}

func TestProbeArgsAndOutcome(t *testing.T) {
	t.Parallel()
	withoutToken := []string{"bash", probeScript, "gastown",
		"--repo", "acme/gastown",
		"--main-branch", "main",
		"--town-root", "/srv/town",
		"--api-url", probeAPIURL,
		"--web-url", probeWebURL,
		"--github-url", probeGitHub,
		"--git-url", probeAdminGit,
	}
	withToken := []string{"bash", probeScript, "gastown",
		"--repo", "acme/gastown",
		"--main-branch", "main",
		"--town-root", "/srv/town",
		"--api-url", probeAPIURL,
		"--web-url", probeWebURL,
		"--admin-token-file", probeTokenFile,
		"--github-url", probeGitHub,
		"--git-url", probeAdminGit,
	}
	tests := []struct {
		name      string
		tokenFile string
		dryRun    bool
		runErr    error
		wantArgv  []string
		wantErr   string
		wantLog   []string
	}{
		{
			name:     "green probe without a token file",
			wantArgv: withoutToken,
			wantLog:  []string{"probing gastown through its real runner before the cutover"},
		},
		{
			name:      "green probe with a token file",
			tokenFile: probeTokenFile,
			wantArgv:  withToken,
			wantLog:   []string{"probing gastown through its real runner before the cutover"},
		},
		{
			name:     "red probe",
			runErr:   errExit,
			wantArgv: withoutToken,
			wantErr:  "the probe of gastown is not green; fix the runner or the gate workflow and re-run",
			wantLog:  []string{"probing gastown through its real runner before the cutover"},
		},
		{
			name:     "dry run prints the probe and runs nothing",
			dryRun:   true,
			wantArgv: nil,
			wantLog: []string{
				"+ " + strings.Join(withoutToken, " "),
				"(dry run: the probe was not run)",
			},
		},
		{
			name:      "dry run with a token file prints the token path and runs nothing",
			tokenFile: probeTokenFile,
			dryRun:    true,
			wantArgv:  nil,
			wantLog: []string{
				"+ " + strings.Join(withToken, " "),
				"(dry run: the probe was not run)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := probeOptions()
			o.AdminTokenFile = tt.tokenFile
			o.DryRun = tt.dryRun
			run := &fakeRunner{err: tt.runErr}
			var logs []string
			err := ProbeGreen(run, o, func(s string) { logs = append(logs, s) })
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ProbeGreen = %v, want nil", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ProbeGreen = %v, want %q", err, tt.wantErr)
			}
			if tt.wantArgv == nil {
				if len(run.calls) != 0 {
					t.Fatalf("dry run ran %q, want no call", run.calls)
				}
			} else if len(run.calls) != 1 || !slices.Equal(run.calls[0], tt.wantArgv) {
				t.Fatalf("ProbeGreen ran %q, want one call of %q", run.calls, tt.wantArgv)
			}
			if !slices.Equal(logs, tt.wantLog) {
				t.Fatalf("logs = %q, want %q", logs, tt.wantLog)
			}
		})
	}
}

// adminMarker is the value of an admin token. It is written into a real token
// file, so the assertions below run against the token's bytes rather than a
// string that never leaves the test.
const adminMarker = "tok-9f3a1c-marker"

// stageTokenFile writes FORGEJO_ADMIN_TOKEN=<marker> to a fresh file and returns
// its path: the value the preflight is handed, never the token itself.
func stageTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.env")
	if err := os.WriteFile(path, []byte("FORGEJO_ADMIN_TOKEN="+adminMarker+"\n"), 0o600); err != nil {
		t.Fatalf("stage the token file: %v", err)
	}
	return path
}

// probeArgvWithToken is the exact probe argv for a rig whose admin token file is
// path. Nothing may be added to it: an extra word is what a leaked token looks
// like, and the slice comparison below turns that red.
func probeArgvWithToken(path string) []string {
	return []string{"bash", probeScript, "gastown",
		"--repo", "acme/gastown",
		"--main-branch", "main",
		"--town-root", "/srv/town",
		"--api-url", probeAPIURL,
		"--web-url", probeWebURL,
		"--admin-token-file", path,
		"--github-url", probeGitHub,
		"--git-url", probeAdminGit,
	}
}

func TestProbeKeepsAdminTokenOutOfArgvLogsAndErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		probe    bool // true runs ProbeGreen; false runs RefuseIfLandingInFlight
		out      string
		runErr   error
		wantArgv func(tokenPath string) []string
	}{
		{
			name:     "green probe",
			probe:    true,
			wantArgv: probeArgvWithToken,
		},
		{
			name:     "probe fails and its exit error echoes the token",
			probe:    true,
			runErr:   fmt.Errorf("exit status 1: %s", adminMarker),
			wantArgv: probeArgvWithToken,
		},
		{
			name:     "probe cannot start with the token in its error",
			probe:    true,
			runErr:   errors.New("fork/exec /usr/bin/bash: " + adminMarker),
			wantArgv: probeArgvWithToken,
		},
		{
			name:     "landing in flight refusal",
			out:      "1\n",
			wantArgv: func(string) []string { return queueArgv },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tokenPath := stageTokenFile(t)
			o := probeOptions()
			o.RigRoot = queueRoot
			o.AdminTokenFile = tokenPath
			run := &fakeRunner{out: []byte(tt.out), err: tt.runErr}
			var logs []string
			log := func(s string) { logs = append(logs, s) }
			var err error
			if tt.probe {
				err = ProbeGreen(run, o, log)
			} else {
				err = RefuseIfLandingInFlight(run, o, log)
			}
			// The token travels as a file path and nowhere else: the recorded argv is
			// the exact slice, and the token's bytes are in no argv word, log line or
			// returned error. Failure messages name the surface, never the value: the
			// bead forbids the token in any test output.
			wantArgv := tt.wantArgv(tokenPath)
			if len(run.calls) != 1 || !slices.Equal(run.calls[0], wantArgv) {
				t.Fatal("a recorded argv is not the exact expected argv")
			}
			for _, argv := range run.calls {
				if strings.Contains(commandText(argv), adminMarker) {
					t.Fatal("a recorded argv carries the admin token")
				}
			}
			for _, line := range logs {
				if strings.Contains(line, adminMarker) {
					t.Fatal("a log line carries the admin token")
				}
			}
			if err != nil && strings.Contains(err.Error(), adminMarker) {
				t.Fatal("the returned error carries the admin token")
			}
		})
	}
}

// TestProbeNeverReadsTheAdminTokenFile pins the other half of the secret rule: the
// preflight hands the path to the probe and never opens the file itself. A path
// that does not exist is therefore still green, which is observable proof of the
// no-read claim without a filesystem seam to count.
func TestProbeNeverReadsTheAdminTokenFile(t *testing.T) {
	t.Parallel()
	o := probeOptions()
	o.AdminTokenFile = filepath.Join(t.TempDir(), "absent.env")
	run := &fakeRunner{}
	if err := ProbeGreen(run, o, func(string) {}); err != nil {
		t.Fatalf("ProbeGreen with an absent token file = %v, want nil", err)
	}
	if len(run.calls) != 1 || !slices.Equal(run.calls[0], probeArgvWithToken(o.AdminTokenFile)) {
		t.Fatalf("ProbeGreen ran %q, want one call of %q", run.calls, probeArgvWithToken(o.AdminTokenFile))
	}
}
