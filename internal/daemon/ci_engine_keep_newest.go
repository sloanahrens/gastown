package daemon

import (
	"slices"
	"strings"
	"time"
)

const (
	// ciEngineBuiltRepoPrefix marks the repositories the CI builds push.
	ciEngineBuiltRepoPrefix = "gastown-"
	// ciEngineBuiltKeepNewest is how many unused images of each built repository stay.
	ciEngineBuiltKeepNewest = 5
)

// ciEngineBuiltExcess names the unused images past the newest ciEngineBuiltKeepNewest
// of every gastown-* repository they are tagged in. An image is named only when it is
// past the cut in each such repository, and never when it is used or protected. A
// protected image still takes its place in the order, so it counts toward the five.
func ciEngineBuiltExcess(images map[string]ciEngineImage, created map[string]time.Time, used map[string]bool, protected []string) map[string]bool {
	byRepo := map[string][]string{}
	for id, img := range images {
		for _, repo := range ciEngineRepoTags(img.RepoTags) {
			if !used[id] && strings.HasPrefix(repo, ciEngineBuiltRepoPrefix) {
				byRepo[repo] = append(byRepo[repo], id)
			}
		}
	}
	// repos counts the built repositories an image is tagged in; past counts those
	// it is past the cut in.
	repos, past := map[string]int{}, map[string]int{}
	for _, ids := range byRepo {
		// Newest first, the id breaking a tie. Equal ids sort together, so Compact drops
		// the repeats of an image tagged twice in one repository.
		slices.SortFunc(ids, func(a, b string) int {
			if c := created[b].Compare(created[a]); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		for i, id := range slices.Compact(ids) {
			repos[id]++
			if i >= ciEngineBuiltKeepNewest {
				past[id]++
			}
		}
	}
	excess := map[string]bool{}
	for id, n := range repos {
		if past[id] == n && !anyOf(ciEngineRepoTags(images[id].RepoTags), protected) {
			excess[id] = true
		}
	}
	return excess
}
