package polecatimage

import (
	"strings"
	"testing"
)

// pruneReply answers the docker calls a tag prune and a build-cache prune make:
// the id of the image :dev names, the id each per-commit tag reports, the tags
// the repo lists, whether the registry holds an alias for a tag, and one failed
// call per key the case names.
type pruneReply struct {
	devID string
	ids   map[string]string // reference -> image id
	tags  result            // docker images
	prune result            // docker builder prune
	alias bool              // the registry holds an alias for a per-commit tag
	fail  map[string]result
}

func (pr *pruneReply) answer(c command) result {
	if res, ok := pr.fail[callKey(c.name, c.args...)]; ok {
		return res
	}
	if c.name != "docker" || len(c.args) == 0 {
		return result{}
	}
	switch {
	case c.args[0] == "images":
		return pr.tags
	case c.args[0] == "builder":
		return pr.prune
	case c.args[0] == "image" && len(c.args) > 2 && c.args[1] == "inspect":
		ref := c.args[len(c.args)-1]
		if ref == "gastown-polecat:dev" {
			return result{stdout: pr.devID + "\n"}
		}
		if strings.HasPrefix(ref, testRegistry+"/") {
			if pr.alias {
				return result{stdout: "sha256:alias\n"}
			}
			return result{exit: 1}
		}
		if id, ok := pr.ids[ref]; ok {
			return result{stdout: id + "\n"}
		}
		return result{exit: 1}
	}
	return result{}
}

// newPruneReply is a repo whose :dev image is dev, with one id per tag.
func newPruneReply(tags ...string) *pruneReply {
	pr := &pruneReply{devID: "sha256:dev", ids: map[string]string{}, fail: map[string]result{}}
	for _, tag := range tags {
		pr.ids["gastown-polecat:"+tag] = "sha256:" + tag
	}
	return pr
}

// TestPruneOldImagesKeepsTheNewestTags is the prune's main path: the newest
// keep tags stay, the rest are untagged one at a time, and :dev and a tag that
// is not a hex commit are never touched (gt-mexpf).
func TestPruneOldImagesKeepsTheNewestTags(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	tr.Cfg.Keep = "2"
	pr := newPruneReply("aaa1111", "bbb2222", "ccc3333", "ddd4444")
	pr.tags = result{stdout: "dev\naaa1111\nlatest\nbbb2222\nccc3333\nddd4444\n123\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.wantCall(t, "docker", "rmi", "gastown-polecat:ccc3333")
	tr.wantCall(t, "docker", "rmi", "gastown-polecat:ddd4444")
	for _, never := range []string{"gastown-polecat:dev", "gastown-polecat:latest", "gastown-polecat:123", "gastown-polecat:aaa1111"} {
		if anyCallNamed(tr.callsTo("docker"), "rmi", never) {
			t.Errorf("the prune untagged %s", never)
		}
	}
	tr.wantLogged(t, "removed gastown-polecat:ccc3333")
}

// TestPruneOldImagesKeepsTheImageDevNames is the guard the tag count alone would
// miss: an old tag that still names the image :dev points at cannot go.
func TestPruneOldImagesKeepsTheImageDevNames(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	tr.Cfg.Keep = "1"
	pr := newPruneReply("aaa1111", "bbb2222", "ccc3333")
	pr.ids["gastown-polecat:ccc3333"] = pr.devID
	pr.tags = result{stdout: "aaa1111\nbbb2222\nccc3333\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "rmi", "gastown-polecat:ccc3333") {
		t.Error("the prune untagged the tag the image :dev names")
	}
}

// TestPruneOldImagesKeepsATagDockerRefuses drives the refusal: docker's reason
// reaches the log, so a container using the image and a real error read
// differently.
func TestPruneOldImagesKeepsATagDockerRefuses(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	tr.Cfg.Keep = "1"
	pr := newPruneReply("aaa1111", "bbb2222")
	pr.tags = result{stdout: "aaa1111\nbbb2222\n"}
	pr.fail[callKey("docker", "rmi", "gastown-polecat:bbb2222")] = result{exit: 1, stderr: "image is being used by a container\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.wantLogged(t, "kept gastown-polecat:bbb2222 (rmi failed: image is being used by a container)")
}

// TestPruneOldImagesDropsTheRegistryAliasFirst pins the order with a registry
// set: the alias is untagged before the local tag, or the image would stay on
// disk.
func TestPruneOldImagesDropsTheRegistryAliasFirst(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Keep = "1"
	pr := newPruneReply("aaa1111", "bbb2222")
	pr.alias = true
	pr.tags = result{stdout: "aaa1111\nbbb2222\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.wantLogged(t, "removed "+testRegistry+"/gastown-polecat:bbb2222")
	if !anyCallNamed(tr.callsTo("docker"), "rmi", "gastown-polecat:bbb2222") {
		t.Error("the local tag stayed after its alias went")
	}
	lines := callLines(tr, "docker")
	aliasAt, tagAt := -1, -1
	for i, line := range lines {
		if line == "docker rmi "+testRegistry+"/gastown-polecat:bbb2222" {
			aliasAt = i
		}
		if line == "docker rmi gastown-polecat:bbb2222" {
			tagAt = i
		}
	}
	if aliasAt < 0 || tagAt < 0 || aliasAt > tagAt {
		t.Errorf("the alias was not untagged before the tag: %v", lines)
	}
}

// TestPruneOldImagesKeepsATagWhoseAliasWillNotGo is the alias refusal: a
// reference docker cannot remove keeps the tag, because dropping the local one
// would leave the image reachable through nothing else.
func TestPruneOldImagesKeepsATagWhoseAliasWillNotGo(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Keep = "1"
	pr := newPruneReply("aaa1111", "bbb2222")
	pr.alias = true
	pr.tags = result{stdout: "aaa1111\nbbb2222\n"}
	pr.fail[callKey("docker", "rmi", testRegistry+"/gastown-polecat:bbb2222")] = result{exit: 1, stderr: "reference does not exist\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.wantLogged(t, "kept gastown-polecat:bbb2222 ("+testRegistry+"/gastown-polecat:bbb2222: rmi failed: reference does not exist)")
	if anyCallNamed(tr.callsTo("docker"), "rmi", "gastown-polecat:bbb2222") {
		t.Error("the tag was untagged after its alias refused to go")
	}
}

// TestPruneOldImagesKeepsEveryTagForAnUnreadableCount is the prune-only knob's
// blast radius: a count that is not a whole number keeps every tag and stops
// before docker is asked anything, so a typo here cannot fail a build or a
// publish (gt-tpg0p.7).
func TestPruneOldImagesKeepsEveryTagForAnUnreadableCount(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Keep = "three"
	tr.reply = newPruneReply().answer
	tr.PruneOldImages()
	tr.wantLogged(t, "images: kept every tag: POLECAT_IMAGE_KEEP=three is not a whole number")
	if anyCallNamed(tr.callsTo("docker"), "images") {
		t.Error("an unreadable count went on to list tags")
	}
	if anyCallNamed(tr.callsTo("docker"), "rmi") {
		t.Error("an unreadable count untagged something")
	}
}

// TestPruneOldImagesLogsAFailedListing is the fail-closed read of the listing:
// a docker that cannot answer prunes nothing and says so.
func TestPruneOldImagesLogsAFailedListing(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	pr := newPruneReply()
	pr.tags = result{exit: 1, stderr: "daemon down"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.wantLogged(t, "images: cannot list the tags of gastown-polecat (daemon down); nothing pruned")
}

// TestPruneOldImagesDoesNothingWhenDockerIsDown pins the first guard: no image
// call follows a failed docker info, so a stopped daemon is not read as an empty
// tag list.
func TestPruneOldImagesDoesNothingWhenDockerIsDown(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = func(c command) result {
		if c.name == "docker" && hasArg(c.args, "info") {
			return result{exit: 1}
		}
		return result{}
	}
	tr.PruneOldImages()
	if got := len(tr.callsTo("docker")); got != 1 {
		t.Errorf("docker was called %d times, want the info probe alone: %v", got, callLines(tr, "docker"))
	}
}

// TestDropRegistryAliasDoesNothingWithoutARegistry pins the step's no-op: no
// registry means no alias to untag.
func TestDropRegistryAliasDoesNothingWithoutARegistry(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	if err := tr.DropRegistryAlias("gastown-polecat:aaa1111"); err != nil {
		t.Fatalf("DropRegistryAlias() = %v, want no work", err)
	}
	if len(tr.calls) != 0 {
		t.Errorf("a run with no registry called %v", callLines(tr, "docker"))
	}
}

// TestDropRegistryAliasLeavesAnAbsentAliasAlone is the common case with a
// registry: a tag that was never published has no alias to remove.
func TestDropRegistryAliasLeavesAnAbsentAliasAlone(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.reply = (&pruneReply{fail: map[string]result{}}).answer
	if err := tr.DropRegistryAlias("gastown-polecat:aaa1111"); err != nil {
		t.Fatalf("DropRegistryAlias() = %v, want no work", err)
	}
	if anyCallNamed(tr.callsTo("docker"), "rmi") {
		t.Error("an absent alias was untagged anyway")
	}
}

func TestKeepCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		n    int
		ok   bool
		name string
	}{
		{"3", 3, true, "a count"},
		{"0", 0, true, "zero, which keeps nothing"},
		{"", 0, false, "unset"},
		{"three", 0, false, "a word"},
		{"-1", 0, false, "a negative count, which would read as keep nothing"},
		{" 3", 0, false, "a padded count"},
	} {
		n, ok := KeepCount(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("KeepCount(%q) = %d, %v, want %d, %v (%s)", tc.in, n, ok, tc.n, tc.ok, tc.name)
		}
	}
}

func TestCapBytes(t *testing.T) {
	t.Parallel()
	const (
		kb = int64(1) << 10
		mb = int64(1) << 20
		gb = int64(1) << 30
	)
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
		name string
	}{
		{"10GB", 10 * gb, true, "gigabytes"},
		{"10G", 10 * gb, true, "the suffix without the byte"},
		{"512M", 512 * mb, true, "megabytes"},
		{"1kB", kb, true, "kilobytes in lower case"},
		{"1024", 1024, true, "a bare byte count"},
		{"0", 0, true, "zero"},
		{"", 0, false, "unset"},
		{"GB", 0, false, "a unit with no count"},
		{"1.5G", 0, false, "a fraction"},
		{"10 GiB", 0, false, "a space and an unknown suffix"},
		{"ten GB", 0, false, "a word"},
		{"-1G", 0, false, "a negative size"},
	} {
		got, ok := CapBytes(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CapBytes(%q) = %d, %v, want %d, %v (%s)", tc.in, got, ok, tc.want, tc.ok, tc.name)
		}
	}
}

// TestCapBytesRefusesACountTooLargeForInt64 is the overflow guard: a figure the
// trim cannot compare is unreadable, not a cap everything fits under.
func TestCapBytesRefusesACountTooLargeForInt64(t *testing.T) {
	t.Parallel()
	if _, ok := CapBytes("99999999999999999999"); ok {
		t.Error("CapBytes accepted a count past int64")
	}
	if _, ok := CapBytes("9999999999P"); ok {
		t.Error("CapBytes accepted a size past int64")
	}
}

func TestHuman(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0.0B"},
		{512, "512.0B"},
		{1536, "1.5KB"},
		{10 * (1 << 30), "10.0GB"},
		{1 << 50, "1.0PB"},
	} {
		if got := Human(tc.in); got != tc.want {
			t.Errorf("Human(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPruneBuildCacheRunsTheBuilderPrune pins the one command the build-cache
// trim runs and the line it logs (gt-atxdz).
func TestPruneBuildCacheRunsTheBuilderPrune(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	pr := newPruneReply()
	pr.prune = result{stdout: "Total reclaimed space: 1.2GB\n"}
	tr.reply = pr.answer
	t.Cleanup(tr.Cleanup)
	tr.PruneBuildCache()
	tr.wantCall(t, "docker", "builder", "prune", "--keep-storage", "8GB", "-f")
	tr.wantLogged(t, "build cache trimmed to the newest 8GB: Total reclaimed space: 1.2GB")
}

// TestPruneBuildCacheSkipsAnotherRunsLock is the trim's lock: a build running
// under another run's lock is never overlapped.
func TestPruneBuildCacheSkipsAnotherRunsLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	tr.reply = newPruneReply().answer
	tr.PruneBuildCache()
	tr.wantLogged(t, "build cache: skipped, another run holds the lock")
	if anyCallNamed(tr.callsTo("docker"), "builder") {
		t.Error("a run that could not take the lock pruned the build cache")
	}
}

// TestPruneBuildCacheLogsAFailedPrune is the failure branch: docker's reason is
// logged and the run carries on.
func TestPruneBuildCacheLogsAFailedPrune(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	pr := newPruneReply()
	pr.prune = result{exit: 1, stderr: "builder is not supported\n"}
	tr.reply = pr.answer
	t.Cleanup(tr.Cleanup)
	tr.PruneBuildCache()
	tr.wantLogged(t, "build cache not trimmed: builder is not supported")
	if strings.Contains(tr.output(), "trimmed to the newest") {
		t.Error("a failed prune was logged as a trim")
	}
}
