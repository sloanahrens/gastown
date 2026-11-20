package daemon

import (
	"maps"
	"testing"
	"time"
)

// keepSpec is one image of a fixture: its id byte, tags, age, and whether it is used.
type keepSpec struct {
	id   byte
	tags []string
	ago  time.Duration
	used bool
}

// keepRun fails unless ciEngineBuiltExcess names exactly the want ids over specs.
func keepRun(t *testing.T, protected []string, want []byte, specs ...keepSpec) {
	t.Helper()
	images, created, used := map[string]ciEngineImage{}, map[string]time.Time{}, map[string]bool{}
	for _, s := range specs {
		id := engImageID(s.id)
		images[id], created[id], used[id] = ciEngineImage{ID: id, RepoTags: s.tags}, ciCacheTestStart.Add(-s.ago), s.used
	}
	wantSet := map[string]bool{}
	for _, b := range want {
		wantSet[engImageID(b)] = true
	}
	if got := ciEngineBuiltExcess(images, created, used, protected); !maps.Equal(got, wantSet) {
		t.Fatalf("named %v, want %v", got, wantSet)
	}
}

// keepRepo returns n unused images of repo, the first ago old and each next an hour newer.
func keepRepo(repo string, first byte, n int, ago time.Duration) []keepSpec {
	var specs []keepSpec
	for i := range n {
		specs = append(specs, keepSpec{id: first + byte(i), tags: []string{repo + ":t"}, ago: ago + time.Duration(i)*time.Hour})
	}
	return specs
}

func TestCIEngineBuiltExcessNamesTheOlderThanFive(t *testing.T) {
	t.Parallel()
	specs := append(keepRepo("gastown-polecat", 0x10, 8, time.Hour), keepRepo("gastown-daemon", 0x20, 8, 30*time.Minute)...)
	keepRun(t, nil, []byte{0x15, 0x16, 0x17, 0x25, 0x26, 0x27}, specs...)
}

func TestCIEngineBuiltExcessSparesWhatItMust(t *testing.T) {
	t.Parallel()
	specs := append(keepRepo("gastown-polecat", 0x10, 6, time.Hour), keepRepo("other", 0x60, 6, time.Hour)...)
	specs = append(specs, keepRepo("gastown-daemon", 0x40, 5, time.Hour)...)
	specs = append(specs,
		keepSpec{id: 0x30, tags: []string{"gastown-polecat:u"}, ago: 100 * time.Hour, used: true},
		keepSpec{id: 0x31, tags: []string{"ci-base:latest"}, ago: 100 * time.Hour},
		// Past the cut in daemon, but tagged in the protected ci-base repository.
		keepSpec{id: 0x50, tags: []string{"gastown-daemon:p", "ci-base:latest"}, ago: 100 * time.Hour},
	)
	keepRun(t, []string{ciEngineBaseRepo}, []byte{0x15}, specs...)
}

func TestCIEngineBuiltExcessTiesAndSharedTags(t *testing.T) {
	t.Parallel()
	// 0x3a and 0x3b share a created time, so the id orders them: 0x3b is the sixth and named.
	ties := append(keepRepo("gastown-polecat", 0x30, 4, time.Hour),
		keepSpec{id: 0x3a, tags: []string{"gastown-polecat:a"}, ago: 10 * time.Hour},
		keepSpec{id: 0x3b, tags: []string{"gastown-polecat:b"}, ago: 10 * time.Hour})
	// The input is a map, so its order varies per call; every call names the same image.
	for range 16 {
		keepRun(t, nil, []byte{0x3b}, ties...)
	}
	// 0x40, 50h old, is tagged in polecat and daemon. Past the cut in polecat, it is
	// within daemon's five when daemon has one newer image, so it is kept there.
	shared := append(keepRepo("gastown-polecat", 0x41, 5, time.Hour),
		keepSpec{id: 0x40, tags: []string{"gastown-polecat:x", "gastown-daemon:x"}, ago: 50 * time.Hour})
	keepRun(t, nil, nil, append(shared, keepRepo("gastown-daemon", 0x46, 1, time.Hour)...)...)
	keepRun(t, nil, []byte{0x40}, append(shared, keepRepo("gastown-daemon", 0x46, 5, time.Hour)...)...)
	// 0x70 is tagged twice in polecat, so it takes one place, not two: the sixth
	// image, 0x75, is the only one past the cut.
	twice := append([]keepSpec{{id: 0x70, tags: []string{"gastown-polecat:a", "gastown-polecat:latest"}, ago: time.Hour}},
		keepRepo("gastown-polecat", 0x71, 5, 2*time.Hour)...)
	keepRun(t, nil, []byte{0x75}, twice...)
}
