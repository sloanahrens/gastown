package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprobe"
	"github.com/steveyegge/gastown/internal/forgejoprov"
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
		GitHubURL:   probeGitHub,
		AdminGitURL: probeAdminGit,
	}
}

// probeArgsWithoutToken is the exact probe argv when no admin token file was
// named: the token reaches the probe through its environment, never in a word.
func probeArgsWithoutToken() []string {
	return []string{"gastown",
		"--repo", "acme/gastown",
		"--main-branch", "main",
		"--town-root", "/srv/town",
		"--api-url", probeAPIURL,
		"--web-url", probeWebURL,
		"--github-url", probeGitHub,
		"--git-url", probeAdminGit,
	}
}

// probeArgsWithToken is the exact probe argv for a rig whose admin token file is
// path. Nothing may be added to it: an extra word is what a leaked token looks
// like, and the slice comparison turns that red.
func probeArgsWithToken(path string) []string {
	return []string{"gastown",
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

// fakeProbe is the ProbeFunc seam: it records each run's arguments and what the
// probe's environment answers for the token variables, and it answers with the
// exit code it is given. The recorded token is only compared, never printed.
type fakeProbe struct {
	rc        int
	calls     [][]string
	brokerTok string // what the probe's getenv answers for the broker's token variable
	standIn   string // what the probe's getenv answers for FORGEJO_ADMIN_TOKEN
	base      string // what the probe's getenv answers for an unrelated variable
}

func (f *fakeProbe) run(_ context.Context, args []string, getenv func(string) string, _ forgejoprobe.Streams) int {
	f.calls = append(f.calls, slices.Clone(args))
	f.brokerTok = getenv(forgejoprov.BrokerTokenEnv)
	f.standIn = getenv(adminTokenEnv)
	f.base = getenv("XDG_CONFIG_HOME")
	return f.rc
}

// probeBaseEnv is the environment the cutover reads: the admin token under both of
// its names, and an unrelated variable the probe must still see.
func probeBaseEnv(name string) string {
	switch name {
	case forgejoprov.BrokerTokenEnv, adminTokenEnv:
		return adminMarker
	case "XDG_CONFIG_HOME":
		return "/srv/config"
	}
	return ""
}

// probeGreenRun runs ProbeGreen over o with a fake probe that answers rc, and
// returns the probe's record, the error and the log lines.
func probeGreenRun(t *testing.T, o *Options, rc int) (*fakeProbe, []string, error) {
	t.Helper()
	probe := &fakeProbe{rc: rc}
	var logs []string
	err := ProbeGreen(context.Background(), probe.run, o, probeToken(t), probeBaseEnv,
		forgejoprobe.Streams{}, func(s string) { logs = append(logs, s) })
	return probe, logs, err
}

// TestProbeGreenRunsInProcess: the probe is run through the ProbeFunc seam, once,
// with the flags and no program word. A probe that went through the exec seam
// would leave this seam unrun and turn the test red.
func TestProbeGreenRunsInProcess(t *testing.T) {
	t.Parallel()
	probe, _, err := probeGreenRun(t, probeOptions(), forgejoprobe.ExitOK)
	if err != nil {
		t.Fatalf("ProbeGreen = %v, want nil", err)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("the probe ran %d times in process, want once", len(probe.calls))
	}
	if !slices.Equal(probe.calls[0], probeArgsWithoutToken()) {
		t.Fatalf("the probe was given %q, want the rig and its flags", probe.calls[0])
	}
	if probe.calls[0][0] == "bash" {
		t.Fatal("the probe's argv names a program: it was started as a child")
	}
}

func TestProbeArgsAndOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tokenFile string
		dryRun    bool
		rc        int
		wantArgs  []string
		wantErr   string
		wantLog   []string
		// wantBroker is true when the probe resolves the token from the broker's
		// variable: no token file was named.
		wantBroker bool
	}{
		{
			name:       "green probe without a token file",
			wantArgs:   probeArgsWithoutToken(),
			wantLog:    []string{"probing gastown through its real runner before the cutover"},
			wantBroker: true,
		},
		{
			name:      "green probe with a token file",
			tokenFile: probeTokenFile,
			wantArgs:  probeArgsWithToken(probeTokenFile),
			wantLog:   []string{"probing gastown through its real runner before the cutover"},
		},
		{
			name:       "red probe",
			rc:         forgejoprobe.ExitFailure,
			wantArgs:   probeArgsWithoutToken(),
			wantErr:    "the probe of gastown is not green; fix the runner or the gate workflow and re-run",
			wantLog:    []string{"probing gastown through its real runner before the cutover"},
			wantBroker: true,
		},
		{
			name:       "a probe that finds its hooks not exercised is red",
			rc:         forgejoprobe.ExitHooksNotExercised,
			wantArgs:   probeArgsWithoutToken(),
			wantErr:    "the probe of gastown is not green; fix the runner or the gate workflow and re-run",
			wantLog:    []string{"probing gastown through its real runner before the cutover"},
			wantBroker: true,
		},
		{
			name:     "dry run prints the probe and runs nothing",
			dryRun:   true,
			wantArgs: nil,
			wantLog: []string{
				"+ forgejo-probe " + strings.Join(probeArgsWithoutToken(), " "),
				"(dry run: the probe was not run)",
			},
		},
		{
			name:      "dry run with a token file prints the token path and runs nothing",
			tokenFile: probeTokenFile,
			dryRun:    true,
			wantArgs:  nil,
			wantLog: []string{
				"+ forgejo-probe " + strings.Join(probeArgsWithToken(probeTokenFile), " "),
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
			probe, logs, err := probeGreenRun(t, o, tt.rc)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ProbeGreen = %v, want nil", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ProbeGreen = %v, want %q", err, tt.wantErr)
			}
			if tt.wantArgs == nil {
				if len(probe.calls) != 0 {
					t.Fatalf("dry run ran the probe %q, want no run", probe.calls)
				}
			} else if len(probe.calls) != 1 || !slices.Equal(probe.calls[0], tt.wantArgs) {
				t.Fatalf("the probe was given %q, want one run of %q", probe.calls, tt.wantArgs)
			}
			if tt.wantBroker {
				if probe.brokerTok != adminMarker {
					t.Fatal("the probe was not handed the resolved token under the broker's name")
				}
			} else if probe.brokerTok != "" {
				t.Fatal("the probe was handed a token it should have read from the named file")
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

// probeToken is the resolved admin token a probe test hands ProbeGreen.
func probeToken(t *testing.T) forgejoprov.Secret {
	t.Helper()
	token, err := forgejoprov.ResolveAdminToken(func(string) string { return adminMarker }, nil, "")
	if err != nil {
		t.Fatalf("resolve the fixture admin token: %v", err)
	}
	return token
}

// TestProbeHandsOnlyTheEnvironmentTokenToTheProbe: with no token file named, the
// probe's environment answers the resolved token under the broker's name and
// nothing under the stand-in name; with a file named, neither token variable is
// answered, and an unrelated variable still is.
func TestProbeHandsOnlyTheEnvironmentTokenToTheProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		tokenFile  string
		wantBroker string
	}{
		{name: "no token file", wantBroker: adminMarker},
		{name: "a token file", tokenFile: probeTokenFile, wantBroker: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := probeOptions()
			o.AdminTokenFile = tt.tokenFile
			probe, _, err := probeGreenRun(t, o, forgejoprobe.ExitOK)
			if err != nil {
				t.Fatal("ProbeGreen failed")
			}
			if probe.brokerTok != tt.wantBroker {
				t.Fatalf("the probe's broker token variable is set = %t, want %t", probe.brokerTok != "", tt.wantBroker != "")
			}
			if probe.standIn != "" {
				t.Fatal("the probe's stand-in token variable is set; it must be masked")
			}
			if probe.base != "/srv/config" {
				t.Fatal("the probe lost an unrelated variable the cutover's environment holds")
			}
		})
	}
}

// TestProbeKeepsAdminTokenOutOfArgvLogsAndErrors: with a token file named, the
// token travels as the file's path and nowhere else: the recorded argv is the exact
// slice, and the token's bytes are in no argv word, log line or returned error, for
// a green probe and a red one.
func TestProbeKeepsAdminTokenOutOfArgvLogsAndErrors(t *testing.T) {
	t.Parallel()
	for _, rc := range []int{forgejoprobe.ExitOK, forgejoprobe.ExitFailure} {
		t.Run(fmt.Sprintf("probe exit %d", rc), func(t *testing.T) {
			t.Parallel()
			tokenPath := stageTokenFile(t)
			o := probeOptions()
			o.AdminTokenFile = tokenPath
			probe, logs, err := probeGreenRun(t, o, rc)
			if len(probe.calls) != 1 || !slices.Equal(probe.calls[0], probeArgsWithToken(tokenPath)) {
				t.Fatal("a recorded probe argv is not the exact expected argv")
			}
			for _, argv := range probe.calls {
				if strings.Contains(strings.Join(argv, " "), adminMarker) {
					t.Fatal("a recorded probe argv carries the admin token")
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

// TestProbeNeverReadsTheAdminTokenFile pins the other half of the secret rule: the
// preflight hands the path to the probe and never opens the file itself. A path
// that does not exist is therefore still green, which is observable proof of the
// no-read claim without a filesystem seam to count.
func TestProbeNeverReadsTheAdminTokenFile(t *testing.T) {
	t.Parallel()
	o := probeOptions()
	o.AdminTokenFile = filepath.Join(t.TempDir(), "absent.env")
	probe, _, err := probeGreenRun(t, o, forgejoprobe.ExitOK)
	if err != nil {
		t.Fatalf("ProbeGreen with an absent token file = %v, want nil", err)
	}
	// The failure message names the surface, never the argv: a mutation that leaked
	// the token would put it in the recorded call, and printing that would leak it
	// into the test output the bead forbids.
	if len(probe.calls) != 1 || !slices.Equal(probe.calls[0], probeArgsWithToken(o.AdminTokenFile)) {
		t.Fatal("a recorded argv is not the exact expected argv")
	}
}
