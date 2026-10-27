package polecatimage

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// volumeReply answers the docker calls a cache trim makes; a case sets the one
// result it is about and leaves the others answered cleanly.
type volumeReply struct {
	inspect result // docker volume inspect
	ps      result // docker ps
	run     result // the helper container
}

// answer is the reply for one call, keyed by the docker subcommand.
func (vr volumeReply) answer(c command) result {
	if c.name != "docker" || len(c.args) == 0 {
		return result{}
	}
	switch c.args[0] {
	case "volume":
		if len(c.args) > 1 && c.args[1] == "inspect" {
			return vr.inspect
		}
	case "ps":
		return vr.ps
	case "run":
		return vr.run
	}
	return result{}
}

// TestTrimCacheVolumeRunsTheHelperReadOnly pins what the program asks docker
// for: a container from the image, read-only, no network, the volume as its one
// mount, and the plugin binary's trim subcommand with the cap in bytes
// (gt-3g7p2).
func TestTrimCacheVolumeRunsTheHelperReadOnly(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{run: result{stdout: "under 1024\n"}}.answer
	tr.TrimCacheVolume("gt-cache-gastown", 10<<30)
	calls := tr.callsTo("docker")
	if len(calls) != 3 {
		t.Fatalf("docker was called %d times, want inspect, ps and run: %v", len(calls), callLines(tr, "docker"))
	}
	want := []string{"run", "--rm", "--read-only", "--network", "none",
		"-v", "gt-cache-gastown:/cache", "gastown-polecat:dev",
		"/usr/local/bin/polecat-image", "trim-cache", "--cap-bytes", strconv.FormatInt(10<<30, 10), "--root", "/cache"}
	if got := strings.Join(calls[2].args, " "); got != strings.Join(want, " ") {
		t.Errorf("the helper ran as %q, want %q", got, strings.Join(want, " "))
	}
	if mounts := countArgs(calls[2].args, "-v"); mounts != 1 {
		t.Errorf("the helper container carries %d mounts, want the volume alone", mounts)
	}
	tr.wantLogged(t, "cache: gt-cache-gastown: 1.0KB of 10GB, under the cap")
}

// countArgs counts how often flag appears among args.
func countArgs(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

func TestTrimCacheVolumeLogsATrim(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{run: result{stdout: "noise\ntrimmed 2048 1024\n"}}.answer
	tr.TrimCacheVolume("gt-cache-gastown", 1024)
	tr.wantLogged(t, "cache: gt-cache-gastown: trimmed 2.0KB to 1.0KB (cap 10GB)")
	if strings.Contains(tr.output(), "still over the cap") {
		t.Error("a trim that reached the cap was reported as still over it")
	}
}

// TestTrimCacheVolumeReportsAVolumeStillOverTheCap pins the second log line: a
// helper that emptied everything and still could not reach the cap says so.
func TestTrimCacheVolumeReportsAVolumeStillOverTheCap(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{run: result{stdout: "trimmed 4096 2048\n"}}.answer
	tr.TrimCacheVolume("gt-cache-gastown", 1024)
	tr.wantLogged(t, "still over the cap with nothing left to trim")
}

// TestTrimCacheVolumeSkipsAVolumeInUse is the safety check: a volume a sandbox
// still has attached is never trimmed, and the container is not started.
func TestTrimCacheVolumeSkipsAVolumeInUse(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{ps: result{stdout: "aaa\nbbb\n"}}.answer
	tr.TrimCacheVolume("gt-cache-gastown", 1024)
	tr.wantLogged(t, "cache: gt-cache-gastown: in use by 2 container(s); skipped")
	if len(tr.callsTo("docker")) != 2 {
		t.Errorf("docker was called %d times, want inspect and ps alone: %v", len(tr.callsTo("docker")), tr.callsTo("docker"))
	}
}

// TestTrimCacheVolumeNamesAProbeItCannotRead drives both probes: an inspect or a
// container query that fails trims nothing and logs which one failed.
func TestTrimCacheVolumeNamesAProbeItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		reply volumeReply
		want  string
	}{
		{"inspect", volumeReply{inspect: result{exit: 1, stderr: "no such volume"}}, "cannot inspect the volume (no such volume)"},
		{"ps", volumeReply{ps: result{exit: 1, stderr: "daemon down"}}, "cannot list the containers using it (daemon down)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.reply = tc.reply.answer
			tr.TrimCacheVolume("gt-cache-gastown", 1024)
			tr.wantLogged(t, tc.want)
			if anyCallNamed(tr.callsTo("docker"), "run") {
				t.Error("a probe that failed still started the helper")
			}
		})
	}
}

// TestTrimCacheVolumeLogsAFailedHelper is the failure branch: the helper's
// reason reaches the log rather than a trim reported as done.
func TestTrimCacheVolumeLogsAFailedHelper(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{run: result{exit: 3, stderr: "trim-cache: cannot measure /cache\n"}}.answer
	tr.TrimCacheVolume("gt-cache-gastown", 1024)
	tr.wantLogged(t, "cache: gt-cache-gastown: not trimmed (trim-cache: cannot measure /cache)")
}

// TestTrimCacheVolumeLogsAnAnswerItCannotRead is the fail-closed read of the
// helper's output: anything but its two line shapes is a trim this run cannot
// vouch for.
func TestTrimCacheVolumeLogsAnAnswerItCannotRead(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{run: result{stdout: "done!\n"}}.answer
	tr.TrimCacheVolume("gt-cache-gastown", 1024)
	tr.wantLogged(t, "not trimmed, the helper printed 'done!'")
}

// TestTrimCacheVolumesTrimsOnlyItsOwnVolumes pins the volume filter's reading:
// only a volume named gt-cache-<rig> is trimmed, and the prefix alone is not a
// rig.
func TestTrimCacheVolumesTrimsOnlyItsOwnVolumes(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	base := volumeReply{run: result{stdout: "under 1\n"}}
	tr.reply = func(c command) result {
		if c.name == "docker" && len(c.args) > 1 && c.args[0] == "volume" && c.args[1] == "ls" {
			return result{stdout: "gt-cache-gastown\nother-volume\ngt-cache-\ngt-cache-om\n"}
		}
		return base.answer(c)
	}
	t.Cleanup(tr.Cleanup)
	tr.TrimCacheVolumes()
	for _, want := range []string{"cache: gt-cache-gastown:", "cache: gt-cache-om:"} {
		tr.wantLogged(t, want)
	}
	if strings.Contains(tr.output(), "cache: other-volume:") || strings.Contains(tr.output(), "cache: gt-cache-:") {
		t.Errorf("a volume outside the cache prefix was trimmed: %s", tr.output())
	}
	tr.wantCall(t, "docker", "volume", "ls", "--format", "{{.Name}}", "--filter", "name=^gt-cache-")
}

// TestTrimCacheVolumesNeedsACapItCanRead drives the cap: one that is not a size
// trims nothing and names the variable, before any volume is listed.
func TestTrimCacheVolumesNeedsACapItCanRead(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.CacheCap = "ten GB"
	tr.reply = volumeReply{}.answer
	tr.TrimCacheVolumes()
	tr.wantLogged(t, "cache: POLECAT_CACHE_CAP=ten GB is not a size such as 10GB; nothing trimmed")
	if anyCallNamed(tr.callsTo("docker"), "ls") {
		t.Error("an unreadable cap went on to list volumes")
	}
}

// TestTrimCacheVolumesTakesAndReleasesItsLock pins the lock that keeps a trim
// off a build: a run that owns none takes it, records its pid, and Cleanup
// removes it.
func TestTrimCacheVolumesTakesAndReleasesItsLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = func(c command) result {
		if c.name == "docker" {
			if len(c.args) > 1 && c.args[0] == "volume" && c.args[1] == "ls" {
				return result{stdout: "gt-cache-gastown\n"}
			}
			return volumeReply{run: result{stdout: "under 1\n"}}.answer(c)
		}
		return result{}
	}
	tr.TrimCacheVolumes()
	if got := lockPID(t, tr.Cfg.LockDir); got != "4242" {
		t.Fatalf("the trim recorded pid %q in the lock, want this run's", got)
	}
	tr.Cleanup()
	if exists(tr.Cfg.LockDir) {
		t.Errorf("the lock survived the run: %s", tr.Cfg.LockDir)
	}
}

// TestTrimCacheVolumesSkipsAnotherRunsLock is the wait path: a lock another run
// holds skips the trim with a logged line and starts no container.
func TestTrimCacheVolumesSkipsAnotherRunsLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = volumeReply{}.answer
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	tr.TrimCacheVolumes()
	tr.wantLogged(t, "cache: skipped, another run holds the lock")
	if anyCallNamed(tr.callsTo("docker"), "run") {
		t.Error("a run that could not take the lock started the helper")
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
		t.Errorf("the other run's lock now records pid %q, want 9999", got)
	}
}

// TestTrimCacheVolumesLogsAListingFailure is the unreadable probe: the whole
// step trims nothing and says why, rather than reading as no volumes.
func TestTrimCacheVolumesLogsAListingFailure(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = func(c command) result {
		if c.name == "docker" && len(c.args) > 1 && c.args[0] == "volume" && c.args[1] == "ls" {
			return result{exit: 1, stderr: "daemon down"}
		}
		return result{}
	}
	t.Cleanup(tr.Cleanup)
	tr.TrimCacheVolumes()
	tr.wantLogged(t, "cache: cannot list the volumes (daemon down); nothing trimmed")
}

// TestOwnLockRejectsALockAnotherRunTook is the takeover window: a run that took
// the lock and then found another run's pid in it may not trim.
func TestOwnLockRejectsALockAnotherRunTook(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	tr.HaveLock = true
	why, err := tr.OwnLock()
	if err != nil {
		t.Fatalf("OwnLock() failed: %v", err)
	}
	if why != "the lock is another run's now" {
		t.Errorf("OwnLock() = %q, want the lock another run took", why)
	}
}

// TestOwnLockReportsALockItCannotTake is the other refusal: a lock path that
// cannot be made an answer, not a panic or a silent trim.
func TestOwnLockReportsALockItCannotTake(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.LockDir = blockedLockDir(t, tr)
	if _, err := tr.OwnLock(); err == nil {
		t.Fatal("OwnLock() = nil, want the lock it could not make")
	}
	if exists(filepath.Join(tr.Cfg.LockDir)) {
		t.Error("the run left a lock behind after failing to take it")
	}
}
