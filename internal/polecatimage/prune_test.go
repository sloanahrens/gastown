package polecatimage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pruneReply answers the docker calls a tag prune, a build-cache prune and a
// dangling-image prune make: the id of the image :dev names, the id each
// per-commit tag reports, the tags the repo lists, whether the registry holds an
// alias for a tag, the size report, and one failed call per key the case names.
type pruneReply struct {
	devID string
	ids   map[string]string // reference -> image id
	tags  result            // docker images
	prune result            // docker builder prune
	image result            // docker image prune
	df    result            // docker system df
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
	case c.args[0] == "system":
		return pr.df
	case c.args[0] == "image" && len(c.args) > 1 && c.args[1] == "prune":
		return pr.image
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

// testSizeReport is a docker system df answer with an Images and a Build Cache
// line, in the template sizeReportFormat reads.
const testSizeReport = "Images 11.4GB 9.9GB (86%)\nBuild Cache 46.2GB 40GB (87%)\n"

// newPruneReply is a repo whose :dev image is dev, with one id per tag, and a
// readable size report.
func newPruneReply(tags ...string) *pruneReply {
	pr := &pruneReply{devID: "sha256:dev", ids: map[string]string{}, fail: map[string]result{}, df: result{stdout: testSizeReport}}
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

// trimContextReply answers a run that trims a named context: the default
// engine reports no per-commit tags, and the context answers its own listing,
// container list and image ids.
type trimContextReply struct {
	context string
	tags    result
	used    result
	ids     map[string]string
	fail    map[string]result
}

func (pr *trimContextReply) answer(c command) result {
	args := c.args
	if c.name != "docker" || len(args) == 0 {
		return result{}
	}
	if res, ok := pr.fail[callKey(c.name, c.args...)]; ok {
		return res
	}
	if len(args) >= 2 && args[0] == "--context" {
		if args[1] != pr.context {
			return result{exit: 1, stderr: "context " + args[1] + " does not exist"}
		}
		args = args[2:]
		switch {
		case len(args) == 0:
			return result{}
		case args[0] == "images":
			return pr.tags
		case args[0] == "ps":
			return pr.used
		case args[0] == "image" && len(args) > 2 && args[1] == "inspect":
			if id, ok := pr.ids[args[len(args)-1]]; ok {
				return result{stdout: id + "\n"}
			}
			return result{exit: 1, stderr: "no such image"}
		}
		return result{}
	}
	if args[0] == "image" {
		return result{exit: 1, stderr: "no such image"}
	}
	return result{}
}

// trimContextRun is a run whose tag prune targets the named context, with four
// tags on it in newest-first order.
func trimContextRun(t *testing.T, context string) (*testRun, *trimContextReply) {
	t.Helper()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	tr.Cfg.Keep = "3"
	tr.Cfg.TrimContext = context
	pr := &trimContextReply{
		context: context,
		tags:    result{stdout: "ddd4444\nccc3333\nbbb2222\naaa1111\n"},
		ids: map[string]string{
			"gastown-polecat:ddd4444": "sha256:ddd",
			"gastown-polecat:ccc3333": "sha256:ccc",
			"gastown-polecat:bbb2222": "sha256:bbb",
			"gastown-polecat:aaa1111": "sha256:aaa",
		},
		fail: map[string]result{},
	}
	tr.reply = pr.answer
	return tr, pr
}

// TestTrimContextRemovesOldTagsOnTheNamedContext is the second trim target: the
// newest three tags stay on the sweep runner's engine and the oldest is
// untagged there (gt-bxp17).
func TestTrimContextRemovesOldTagsOnTheNamedContext(t *testing.T) {
	t.Parallel()
	tr, _ := trimContextRun(t, "colima-gt-control")
	tr.PruneOldImages()
	tr.wantCall(t, "docker", "--context", "colima-gt-control", "rmi", "gastown-polecat:aaa1111")
	for _, kept := range []string{"ddd4444", "ccc3333", "bbb2222"} {
		if anyCallNamed(tr.callsTo("docker"), "rmi", "gastown-polecat:"+kept) {
			t.Errorf("the context prune untagged the kept %s", kept)
		}
	}
	tr.wantLogged(t, "removed gastown-polecat:aaa1111 on context colima-gt-control")
}

// TestTrimContextSkipsAnImageInUse is the safety rule on the second engine: a
// container, running or stopped, keeps the image it runs (gt-bxp17).
func TestTrimContextSkipsAnImageInUse(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	pr.used = result{stdout: "gastown-polecat:aaa1111\n"}
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi") {
		t.Errorf("an image a container uses was untagged: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "kept gastown-polecat:aaa1111 on context colima-gt-control (in use by a container)")
}

// TestTrimContextSkipsAnImageAContainerNamesByRegistryRef covers a container
// created from a registry-prefixed reference: docker ps names the whole repo,
// so the tag it selects is matched by its last path segment (gt-bxp17).
func TestTrimContextSkipsAnImageAContainerNamesByRegistryRef(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	pr.used = result{stdout: "colima-registry.local:5000/gastown-polecat:aaa1111\n"}
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi") {
		t.Errorf("an image a container uses by registry reference was untagged: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "kept gastown-polecat:aaa1111 on context colima-gt-control (in use by a container)")
}

// TestTrimContextSkipsAnImageAContainerNamesByID covers docker ps reporting the
// image id rather than the tag the container was created from.
func TestTrimContextSkipsAnImageAContainerNamesByID(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	pr.ids["gastown-polecat:aaa1111"] = "sha256:0123456789ab"
	pr.used = result{stdout: "0123456789ab\n"}
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi") {
		t.Errorf("an image a container names by id was untagged: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "kept gastown-polecat:aaa1111 on context colima-gt-control (in use by a container)")
}

// TestTrimContextDropsTheRegistryAliasFirst keeps the second engine's trim the
// same trim: an alias tag loaded beside the image would keep it on disk, so it
// is untagged there before the per-commit tag, with the context named
// (gt-bxp17).
func TestTrimContextDropsTheRegistryAliasFirst(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	tr.Cfg.Registry = testRegistry
	alias := testRegistry + "/gastown-polecat:aaa1111"
	pr.ids[alias] = "sha256:aaa"
	tr.PruneOldImages()
	tr.wantCall(t, "docker", "--context", "colima-gt-control", "rmi", alias)
	tr.wantCall(t, "docker", "--context", "colima-gt-control", "rmi", "gastown-polecat:aaa1111")
	aliasAt := callPosition(tr, "--context", "colima-gt-control", "rmi", alias)
	tagAt := callPosition(tr, "--context", "colima-gt-control", "rmi", "gastown-polecat:aaa1111")
	if aliasAt < 0 || tagAt < 0 || aliasAt > tagAt {
		t.Errorf("alias untagged at %d, tag at %d; want the alias first: %v", aliasAt, tagAt, callLines(tr, "docker"))
	}
	tr.wantLogged(t, "removed "+alias+" on context colima-gt-control")
}

// TestTrimContextKeepsATagWhoseAliasWillNotGo is the alias rule on the second
// engine: a tag whose alias docker refuses to untag is kept, so the image is
// never left on disk with no tag naming it (gt-bxp17).
func TestTrimContextKeepsATagWhoseAliasWillNotGo(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	tr.Cfg.Registry = testRegistry
	alias := testRegistry + "/gastown-polecat:aaa1111"
	pr.ids[alias] = "sha256:aaa"
	pr.fail[callKey("docker", "--context", "colima-gt-control", "rmi", alias)] = result{exit: 1, stderr: "image is in use"}
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi", "gastown-polecat:aaa1111") {
		t.Errorf("a tag whose alias would not go was untagged: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "kept gastown-polecat:aaa1111 on context colima-gt-control ("+alias+": rmi failed: image is in use)")
}

// callPosition is the index of the docker call carrying exactly want, -1 when
// there is none.
func callPosition(tr *testRun, want ...string) int {
	for i, c := range tr.callsTo("docker") {
		if callKey(c.name, c.args...) == callKey("docker", want...) {
			return i
		}
	}
	return -1
}

// TestTrimContextPrunesNothingWhenTheEngineIsUnreachable names the engine's
// own failure: a context docker cannot reach prunes nothing and says so
// (gt-bxp17).
func TestTrimContextPrunesNothingWhenTheEngineIsUnreachable(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	pr.fail[callKey("docker", "--context", "colima-gt-control", "info")] = result{exit: 1, stderr: "no such context"}
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi") {
		t.Errorf("an unreachable engine was pruned: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "images on context colima-gt-control: engine unreachable (no such context); nothing pruned")
}

// TestTrimContextPrunesNothingWhenContainersCannotBeRead is the fail-closed
// probe: a container list the plugin cannot read never reads as "no container
// uses these" (gt-bxp17).
func TestTrimContextPrunesNothingWhenContainersCannotBeRead(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	pr.fail[callKey("docker", "--context", "colima-gt-control", "ps", "-a", "--format", containersFormat)] = result{exit: 1, stderr: "engine unreachable"}
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi") {
		t.Errorf("a run that could not read the containers pruned anyway: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "images on context colima-gt-control: cannot list the containers (engine unreachable); nothing pruned")
}

// TestTrimContextKeepsATagWhoseImageIDCannotBeRead is the same rule per tag: an
// id the plugin cannot read is not a candidate for removal (gt-bxp17).
func TestTrimContextKeepsATagWhoseImageIDCannotBeRead(t *testing.T) {
	t.Parallel()
	tr, pr := trimContextRun(t, "colima-gt-control")
	delete(pr.ids, "gastown-polecat:aaa1111")
	tr.PruneOldImages()
	if anyCallNamed(tr.callsTo("docker"), "--context", "colima-gt-control", "rmi") {
		t.Errorf("a tag whose image id was unreadable was untagged: %v", callLines(tr, "docker"))
	}
	tr.wantLogged(t, "kept gastown-polecat:aaa1111 on context colima-gt-control (cannot read its image id: no such image)")
}

// TestTrimContextUnsetKeepsTodaysBehaviour pins the single-engine run: with
// POLECAT_IMAGE_TRIM_CONTEXT unset, no call names a context and the default
// engine is trimmed exactly as before (gt-bxp17).
func TestTrimContextUnsetKeepsTodaysBehaviour(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Cfg.Registry = ""
	tr.Cfg.Keep = "1"
	pr := newPruneReply("aaa1111", "bbb2222")
	pr.tags = result{stdout: "aaa1111\nbbb2222\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.wantCall(t, "docker", "rmi", "gastown-polecat:bbb2222")
	tr.wantLogged(t, "removed gastown-polecat:bbb2222")
	if strings.Contains(tr.output(), "on context") {
		t.Errorf("a run with no trim context named one: %s", tr.output())
	}
	for _, c := range tr.callsTo("docker") {
		if hasArg(c.args, "--context") {
			t.Errorf("a run with no trim context called docker with --context: %v", callLines(tr, "docker"))
			break
		}
	}
}

// TestPruneOldImagesRefusesAnUnsafeTrimContext is the input guard: a value
// that is not one Docker context name stops that trim and never reaches
// docker's argv (gt-bxp17).
func TestPruneOldImagesRefusesAnUnsafeTrimContext(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"-rf", "colima/gt", "colima:gt", "colima gt", ".."} {
		tr := newTestRun(t)
		tr.Cfg.TrimContext = name
		tr.PruneOldImages()
		for _, c := range tr.callsTo("docker") {
			if hasArg(c.args, "--context") {
				t.Errorf("the context %q reached docker: %v", name, callLines(tr, "docker"))
				break
			}
		}
		tr.wantLogged(t, "images on context "+name+": not trimmed: not one Docker context name")
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

// TestPruneBuildCacheDoesNothingWhenDockerIsDown pins the same first guard as
// the dangling-image prune: a failed docker info stops the trim before the lock
// or any size read, so a stopped daemon is never read as an empty cache (gt-n0tfr).
func TestPruneBuildCacheDoesNothingWhenDockerIsDown(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	pr := newPruneReply()
	pr.fail[callKey("docker", "info")] = result{exit: 1, stderr: "daemon down"}
	tr.reply = pr.answer
	tr.PruneBuildCache()
	if got := len(tr.callsTo("docker")); got != 1 {
		t.Errorf("docker was called %d times, want the info probe alone: %v", got, callLines(tr, "docker"))
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

// forbiddenPrune reports whether a docker call is a prune this plugin may never
// run: a system or volume prune, or any prune with --all or a short -a flag,
// which reach past dangling images (gt-atxdz, gt-n0tfr).
func forbiddenPrune(args []string) bool {
	if !hasArg(args, "prune") {
		return false
	}
	for _, a := range args {
		switch {
		case a == "system", a == "volume", a == "--all":
			return true
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'a'):
			return true
		}
	}
	return false
}

// TestPruneDanglingRunsOnlyTheDanglingForm pins the one image prune the plugin
// runs: image prune -f, once, with no flag that reaches past dangling images
// (gt-n0tfr).
func TestPruneDanglingRunsOnlyTheDanglingForm(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	tr.reply = newPruneReply().answer
	tr.PruneDanglingImages()
	imagePrunes := 0
	for _, c := range tr.callsTo("docker") {
		if len(c.args) > 0 && c.args[0] == "image" && hasArg(c.args, "prune") {
			imagePrunes++
		}
		if forbiddenPrune(c.args) {
			t.Errorf("a forbidden prune ran: %v", callLines(tr, "docker"))
		}
	}
	if imagePrunes != 1 {
		t.Errorf("image prune ran %d times, want once: %v", imagePrunes, callLines(tr, "docker"))
	}
	tr.wantCall(t, "docker", "image", "prune", "-f")
}

// TestPruneDanglingDoesNothingWhenDockerIsDown pins the first guard: no image
// prune or size read follows a failed docker info, so a stopped daemon is never
// read as an empty image list (gt-n0tfr).
func TestPruneDanglingDoesNothingWhenDockerIsDown(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	pr := newPruneReply()
	pr.fail[callKey("docker", "info")] = result{exit: 1, stderr: "daemon down"}
	tr.reply = pr.answer
	tr.PruneDanglingImages()
	if got := len(tr.callsTo("docker")); got != 1 {
		t.Errorf("docker was called %d times, want the info probe alone: %v", got, callLines(tr, "docker"))
	}
}

// TestPruneDanglingSkipsWithoutTheLock is the trim's lock: a dangling-image
// prune under another run's lock never overlaps a build (gt-n0tfr).
func TestPruneDanglingSkipsWithoutTheLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	tr.reply = newPruneReply().answer
	tr.PruneDanglingImages()
	tr.wantLogged(t, "dangling images: skipped, another run holds the lock")
	if anyCallNamed(tr.callsTo("docker"), "image", "prune") {
		t.Error("a run that could not take the lock pruned dangling images")
	}
}

// TestPruneDanglingLogsAnUnreadableLock is the lock's error branch: a lock
// directory that cannot be created is logged with its reason, and no image
// prune runs under a lock this run does not hold (gt-n0tfr). The runtime
// directory is replaced by a file, so the lock's parent cannot be made.
func TestPruneDanglingLogsAnUnreadableLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	runtime := filepath.Dir(tr.Cfg.LockDir)
	if err := os.RemoveAll(runtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtime, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.reply = newPruneReply().answer
	tr.PruneDanglingImages()
	tr.wantLogged(t, "dangling images: not pruned: ")
	tr.wantLogged(t, "not a directory")
	if anyCallNamed(tr.callsTo("docker"), "image", "prune") {
		t.Error("a run that could not read its lock pruned dangling images")
	}
}

// TestPruneDanglingFailureIsLoggedNotFatal is the failure branch: docker's
// reason is logged, and the run carries on to the build-cache trim after it
// (gt-n0tfr).
func TestPruneDanglingFailureIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	pr := newPruneReply()
	pr.image = result{exit: 1, stderr: "permission denied\n"}
	tr.reply = pr.answer
	tr.PruneDanglingImages()
	tr.wantLogged(t, "dangling images not pruned: permission denied")
	if strings.Contains(tr.output(), "dangling images pruned:") {
		t.Error("a failed prune was logged as pruned")
	}
	tr.PruneBuildCache()
	tr.wantCall(t, "docker", "builder", "prune", "--keep-storage", "8GB", "-f")
}

// TestPrunesLogSizesBeforeAndAfter pins the size line each prune logs: the kind
// it trims, read from docker's size report before and after the prune. A report
// that cannot be read is logged with its reason, and the prune still runs
// (gt-n0tfr).
func TestPrunesLogSizesBeforeAndAfter(t *testing.T) {
	t.Parallel()
	const after = "Images 1.5GB 0B (0%)\nBuild Cache 8.0GB 0B (0%)\n"
	const unreadable = "cannot reach the engine\n"
	for _, tc := range []struct {
		name  string
		prune func(*testRun)
		argv  []string
		reads []result // the size report answers, in call order: before, then after
		want  string
	}{
		{
			name:  "build cache readable",
			prune: func(tr *testRun) { tr.PruneBuildCache() },
			argv:  []string{"builder", "prune", "--keep-storage", "8GB", "-f"},
			reads: []result{{stdout: testSizeReport}, {stdout: after}},
			want:  "Build Cache size: 46.2GB before, 8.0GB after",
		},
		{
			name:  "dangling images readable",
			prune: func(tr *testRun) { tr.PruneDanglingImages() },
			argv:  []string{"image", "prune", "-f"},
			reads: []result{{stdout: testSizeReport}, {stdout: after}},
			want:  "Images size: 11.4GB before, 1.5GB after",
		},
		{
			name:  "build cache unreadable",
			prune: func(tr *testRun) { tr.PruneBuildCache() },
			argv:  []string{"builder", "prune", "--keep-storage", "8GB", "-f"},
			reads: []result{{exit: 1, stderr: unreadable}, {exit: 1, stderr: unreadable}},
			want:  "Build Cache size: unknown before, unknown after (size report unreadable: docker system df failed: cannot reach the engine)",
		},
		{
			name:  "dangling images unreadable",
			prune: func(tr *testRun) { tr.PruneDanglingImages() },
			argv:  []string{"image", "prune", "-f"},
			reads: []result{{exit: 1, stderr: unreadable}, {exit: 1, stderr: unreadable}},
			want:  "Images size: unknown before, unknown after (size report unreadable: docker system df failed: cannot reach the engine)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestRun(t)
			t.Cleanup(tr.Cleanup)
			pr := newPruneReply()
			reads := tc.reads
			tr.reply = func(c command) result {
				if c.name == "docker" && len(c.args) > 0 && c.args[0] == "system" && len(reads) > 0 {
					next := reads[0]
					reads = reads[1:]
					return next
				}
				return pr.answer(c)
			}
			tc.prune(tr)
			tr.wantLogged(t, tc.want)
			tr.wantCall(t, "docker", tc.argv...)
		})
	}
}

// TestNoOtherPruneFormIsAllowed runs each step of a trim run and checks every
// docker call it makes: the two prunes it may run are the only prune forms, and
// the forms it may never run are refused by the same check (gt-atxdz, gt-n0tfr).
func TestNoOtherPruneFormIsAllowed(t *testing.T) {
	t.Parallel()
	for _, form := range [][]string{
		{"system", "prune"},
		{"system", "prune", "-a"},
		{"volume", "prune"},
		{"image", "prune", "-a"},
		{"image", "prune", "--all"},
		{"image", "prune", "-af"},
		{"builder", "prune", "--all"},
	} {
		if !forbiddenPrune(form) {
			t.Errorf("the check lets %v through", form)
		}
	}
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	tr.Cfg.Keep = "1"
	pr := newPruneReply("aaa1111", "bbb2222")
	pr.tags = result{stdout: "aaa1111\nbbb2222\n"}
	tr.reply = pr.answer
	tr.PruneOldImages()
	tr.PruneBuildCache()
	tr.PruneDanglingImages()
	for _, c := range tr.callsTo("docker") {
		if forbiddenPrune(c.args) {
			t.Errorf("a trim run made a forbidden prune: %v", callLines(tr, "docker"))
		}
	}
	tr.wantCall(t, "docker", "builder", "prune", "--keep-storage", "8GB", "-f")
	tr.wantCall(t, "docker", "image", "prune", "-f")
}
