package uninstallgt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// host is a temporary host: a home with its live install directory and a town
// with a town marker. Every path and every variable in this file comes from the
// case, so no test reads the developer's home or the town this clone belongs to.
type host struct {
	home string
	live string
	town string
}

func newHost(t *testing.T) host {
	t.Helper()
	base := t.TempDir()
	h := host{
		home: filepath.Join(base, "home"),
		town: filepath.Join(base, "town"),
	}
	h.live = filepath.Join(h.home, ".local", "bin")
	mustMkdir(t, h.live)
	mustMkdir(t, filepath.Join(h.town, "mayor"))
	mustWrite(t, filepath.Join(h.town, "mayor", "town.json"), "{}\n")
	return h
}

// daemon writes the town's pid file in the daemon's "PID\nNONCE" format.
func (h host) daemon(t *testing.T, content string) {
	t.Helper()
	mustMkdir(t, filepath.Join(h.town, "daemon"))
	mustWrite(t, filepath.Join(h.town, "daemon", "daemon.pid"), content)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// neverSignals is the Signal0 seam for a case that expects no probe.
func neverSignals(int) bool { return false }

// neverTown is a marker check for a tree with no town in it.
func neverTown(string) bool { return false }

func TestTownRootDetection(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	town := filepath.Join(base, "town")
	rigMayor := filepath.Join(town, "gastown")
	stranger := filepath.Join(base, "not-a-town")
	// The marker check is the case's own, so whatever sits above the test's
	// temporary tree cannot change the answer.
	isTown := func(dir string) bool { return dir == town || dir == rigMayor }

	tests := []struct {
		name    string
		vars    map[string]string
		selfDir string
		want    string
	}{
		{
			name:    "GT_TOWN_ROOT names the town",
			vars:    map[string]string{"GT_TOWN_ROOT": town},
			selfDir: filepath.Join(stranger, "bin"),
			want:    town,
		},
		{
			name: "GT_TOWN_ROOT with a trailing slash",
			vars: map[string]string{"GT_TOWN_ROOT": town + "/"},
			want: town,
		},
		{
			name:    "the outermost town above the binary's directory",
			selfDir: filepath.Join(rigMayor, "bin"),
			want:    town,
		},
		{
			name:    "no marker above the binary's directory",
			selfDir: filepath.Join(stranger, "bin"),
			want:    "",
		},
		{
			// A caller points a run at a scratch tree this way, so the value is
			// no town rather than an error, and the search above the binary is
			// not consulted in its place.
			name:    "GT_TOWN_ROOT without a marker is no town",
			vars:    map[string]string{"GT_TOWN_ROOT": stranger},
			selfDir: filepath.Join(town, "gastown", "bin"),
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTown(vars(tt.vars), tt.selfDir, isTown, neverSignals)
			if got.Root != tt.want {
				t.Errorf("detectTown(%v, %q) town = %q, want %q", tt.vars, tt.selfDir, got.Root, tt.want)
			}
		})
	}
}

// TestHomeGtFallbackIsATown pins gt-bqzoq: a clone outside the town, with
// GT_TOWN_ROOT unset, still finds the conventional town in HOME and refuses the
// install that answers for the host.
func TestHomeGtFallbackIsATown(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	conventional := filepath.Join(h.home, "gt")
	mustMkdir(t, filepath.Join(conventional, "mayor"))
	mustWrite(t, filepath.Join(conventional, "mayor", "town.json"), "{}\n")

	town := detectTown(vars(map[string]string{"HOME": h.home}),
		filepath.Join(filepath.Dir(h.home), "clone", "bin"),
		func(dir string) bool { return dir == conventional }, neverSignals)

	if town.Root != conventional {
		t.Fatalf("detectTown with no GT_TOWN_ROOT and no town above the binary = %q, want %q", town.Root, conventional)
	}
	if _, err := town.CheckLive(h.live, h.home, false); err == nil {
		t.Errorf("CheckLive(%q) accepted the live install of the home town %q", h.live, conventional)
	}
}

func TestNoTownMeansNoRefusal(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	town := detectTown(vars(map[string]string{
		"GT_TOWN_ROOT": filepath.Join(filepath.Dir(h.town), "not-a-town"),
		"HOME":         h.home,
	}), h.live, func(dir string) bool { return dir == h.town }, neverSignals)

	if town.Root != "" {
		t.Fatalf("detectTown found the town %q, want none", town.Root)
	}
	notice, err := town.CheckLive(h.live, h.home, false)
	if err != nil {
		t.Errorf("CheckLive(%q) with no town refused: %v", h.live, err)
	}
	if notice != nil {
		t.Errorf("CheckLive(%q) with no town printed %q, want nothing", h.live, notice)
	}
}

func TestDaemonRunningProbesThePidFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		pid   string // "" writes no pid file
		alive bool
		want  bool
		asked int // 0: the probe must not be made
	}{
		{name: "an alive pid", pid: "4242\nnonce\n", alive: true, want: true, asked: 4242},
		{name: "a pid that is gone", pid: "4242\nnonce\n", alive: false, want: false, asked: 4242},
		{name: "the first line is the pid, not the nonce", pid: "4242\n5150\n", alive: true, want: true, asked: 4242},
		{name: "digits among other characters", pid: "pid=4242\n\n", alive: true, want: true, asked: 4242},
		{name: "no pid file", pid: "", alive: true, want: false, asked: 0},
		{name: "an empty pid file", pid: "\nnonce\n", alive: true, want: false, asked: 0},
		{name: "a pid of 0 names the process group", pid: "0\nnonce\n", alive: true, want: false, asked: 0},
		{name: "no pid in the file", pid: "not-a-pid\n", alive: true, want: false, asked: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t)
			if tt.pid != "" {
				h.daemon(t, tt.pid)
			}
			var asked []int
			signal0 := func(pid int) bool {
				asked = append(asked, pid)
				return tt.alive
			}

			if got := DaemonRunning(h.town, signal0); got != tt.want {
				t.Errorf("DaemonRunning(%q) = %t, want %t", h.town, got, tt.want)
			}
			if tt.asked == 0 {
				if len(asked) != 0 {
					t.Errorf("Signal0 asked about %v, want no probe", asked)
				}
			} else if len(asked) != 1 || asked[0] != tt.asked {
				t.Errorf("Signal0 asked about %v, want [%d]", asked, tt.asked)
			}
		})
	}
}

func TestDaemonRunningHintOnlyWhenRunning(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.daemon(t, "4242\nnonce\n")
	getenv := vars(map[string]string{"GT_TOWN_ROOT": h.town, "HOME": h.home})
	isTown := func(dir string) bool { return dir == h.town }

	running := detectTown(getenv, "", isTown, func(pid int) bool { return pid == 4242 })
	if !running.DaemonRunning {
		t.Fatalf("detectTown = %+v, want a running daemon", running)
	}
	_, err := running.CheckLive(h.live, h.home, false)
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("CheckLive(%q) error = %v, want a *Refusal", h.live, err)
	}
	if hint := "the daemon is running now: stop it first with 'gt daemon stop'."; !strings.Contains(refusal.Reason, hint) {
		t.Errorf("the refusal of a running town's install = %q, want it to carry %q", refusal.Reason, hint)
	}

	gone := detectTown(getenv, "", isTown, func(int) bool { return false })
	if gone.DaemonRunning {
		t.Fatalf("detectTown = %+v, want no running daemon", gone)
	}
	_, err = gone.CheckLive(h.live, h.home, false)
	if !errors.As(err, &refusal) {
		t.Fatalf("CheckLive(%q) error = %v, want a *Refusal", h.live, err)
	}
	if strings.Contains(refusal.Reason, "daemon is running now") {
		t.Errorf("the refusal of a stopped town's install = %q, want no daemon hint", refusal.Reason)
	}
}

func TestLiveInstallRefusedWithoutForce(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	target, err := Resolve(Options{}, vars(map[string]string{"HOME": h.home}))
	if err != nil {
		t.Fatalf("Resolve with HOME=%q: %v", h.home, err)
	}
	town := Town{Root: h.town, DaemonRunning: true}

	notice, err := town.CheckLive(target.Dir, h.home, false)

	if notice != nil {
		t.Errorf("CheckLive(%q) printed %q beside the refusal, want nothing", target.Dir, notice)
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("CheckLive(%q) error = %v, want a *Refusal", target.Dir, err)
	}
	for _, want := range []string{
		target.Dir + " is the live install of the town at " + h.town,
		"gt mail, gt escalate and the daemon's own escalation",
		"all shell out to bd",
		"stop it first with 'gt daemon stop'",
		"make uninstall FORCE=1",
		`    uninstall-gt --install-dir "$(mktemp -d)"`,
	} {
		if !strings.Contains(refusal.Reason, want) {
			t.Errorf("the refusal of %q = %q, want it to carry %q", target.Dir, refusal.Reason, want)
		}
	}
	if ExitRefused != 2 {
		t.Errorf("ExitRefused = %d, want the script's 2", ExitRefused)
	}
}

// TestSymlinkAliasOfTheLiveDirIsRefused is the incident's other shape: a
// ~/bin -> ~/.local/bin from an older setup names the live install, and a
// spelling that resolved to itself would remove the live binaries through it.
func TestSymlinkAliasOfTheLiveDirIsRefused(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	alias := filepath.Join(h.home, "bin")
	if err := os.Symlink(h.live, alias); err != nil {
		t.Fatal(err)
	}
	homeLink := filepath.Join(filepath.Dir(h.home), "home-link")
	if err := os.Symlink(h.home, homeLink); err != nil {
		t.Fatal(err)
	}
	town := Town{Root: h.town}

	for _, dir := range []string{alias, filepath.Join(homeLink, ".local", "bin"), h.live + "/"} {
		_, err := town.CheckLive(dir, h.home, false)
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("CheckLive(%q) error = %v, want the live-install refusal", dir, err)
		}
		if want := dir + " is the live install of the town at " + h.town; !strings.Contains(refusal.Reason, want) {
			t.Errorf("CheckLive(%q) refused with %q, want it to name %q", dir, refusal.Reason, want)
		}
	}
}

func TestLiveScratchDirectoryIsNotRefused(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	town := Town{Root: h.town, DaemonRunning: true}
	scratch := filepath.Join(filepath.Dir(h.live), "scratch")

	notice, err := town.CheckLive(scratch, h.home, false)

	if err != nil {
		t.Errorf("CheckLive(%q), a scratch directory beside the live install, refused: %v", scratch, err)
	}
	if notice != nil {
		t.Errorf("CheckLive(%q) printed %q, want nothing", scratch, notice)
	}
}

func TestForceAllowsButWarns(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	tests := []struct {
		name      string
		running   bool
		wantLines int
	}{
		{name: "the daemon is running", running: true, wantLines: 2},
		{name: "the daemon is gone", running: false, wantLines: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := Town{Root: h.town, DaemonRunning: tt.running}

			notice, err := town.CheckLive(h.live, h.home, true)

			if err != nil {
				t.Fatalf("CheckLive(%q, --force) error = %v, want none", h.live, err)
			}
			if len(notice) != tt.wantLines {
				t.Fatalf("CheckLive(%q, --force) printed %q, want %d lines", h.live, notice, tt.wantLines)
			}
			if want := "FORCE=1 — removing the live install of the town at " + h.town; !strings.Contains(notice[0], want) {
				t.Errorf("the announcement = %q, want it to carry %q", notice[0], want)
			}
			if got := strings.Contains(strings.Join(notice, "\n"), "still running"); got != tt.running {
				t.Errorf("the announcement of a daemon running=%t = %q, want the warning=%t", tt.running, notice, tt.running)
			}
		})
	}
}

// TestLiveInstallNeedsHome pins the guard for a run with no HOME: Resolve has
// already refused it, and ./.local/bin beside the working directory is not the
// live install of anything.
func TestLiveInstallNeedsHome(t *testing.T) {
	t.Parallel()
	town := Town{Root: "/town"}
	if _, err := town.CheckLive(filepath.Join(".local", "bin"), "", false); err != nil {
		t.Errorf("CheckLive with HOME empty refused: %v", err)
	}
}

func TestCanonical(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir := filepath.Join(base, "bin")
	mustMkdir(t, dir)
	link := filepath.Join(base, "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{name: "an existing directory", dir: dir, want: resolved},
		{name: "a trailing slash", dir: dir + "/", want: resolved},
		{name: "a . inside the path", dir: filepath.Join(dir, "."), want: resolved},
		{name: "a symlink", dir: link, want: resolved},
		{
			name: "an absent path keeps its cleaned spelling",
			dir:  filepath.Join(base, "absent", "bin", "..", "bin"),
			want: filepath.Join(base, "absent", "bin"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalPath(tt.dir); got != tt.want {
				t.Errorf("canonicalPath(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}
