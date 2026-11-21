package daemonimage

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// devopsToolDirs are the devops commands the image builds (tools.go).
var devopsToolDirs = []string{"cmd/reporter", "cmd/release-reporter", "cmd/cloudcheck"}

// maxWalkDirs bounds the closure walk: the walk reads at most this many
// directories and refuses the next one (gt-tly3py).
const maxWalkDirs = 200

// importGap is a devops directory a tool imports that the input list lacks, and
// the tool that imports it.
type importGap struct {
	Dir, Importer string
}

// inputClosureGaps walks the devops imports breadth first from each tool at
// main through git and returns each directory it reaches that
// PinInputPaths("devops") does not cover, with the directory that imported it:
// a deeper import (pkg/patrol importing the unlisted pkg/other) names
// pkg/patrol. It reads the non-test .go files of each directory. dir is the
// root of the pin's checkout (MainOf in CheckPins, checkFetchedPin), so the
// names that ls-tree prints are repository paths, valid in `main:<file>`. A
// failed read or a source that does not parse is an error, not a clean list,
// and so is a directory with no non-test .go file at main: an empty listing
// would pass a renamed or removed directory silently (gt-reirlx). The walk
// queues every import, covered or not, and reads each directory once (gt-rblgfn),
// so it does not rely on Go forbidding cycles. Past maxWalkDirs directories the
// walk stops with an error rather than returning a partial list (gt-tly3py).
func inputClosureGaps(git bdbuild.Git, dir, main string) ([]importGap, error) {
	type visit struct{ dir, importer string }
	var queue []visit
	for _, tool := range devopsToolDirs {
		queue = append(queue, visit{dir: tool})
	}
	var gaps []importGap
	visited := make(map[string]bool)
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if visited[v.dir] {
			continue
		}
		if len(visited) >= maxWalkDirs {
			return nil, fmt.Errorf("pin-drift guard: the walk passed %d directories at %s: the list is too deep to check", maxWalkDirs, v.dir)
		}
		visited[v.dir] = true
		listing, err := git(dir, "ls-tree", "--name-only", main, v.dir+"/")
		if err != nil {
			return nil, err
		}
		sources := 0
		for _, file := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
			if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
				continue
			}
			sources++
			src, err := git(dir, "show", main+":"+file)
			if err != nil {
				return nil, err
			}
			imports, err := devopsImports(string(src))
			if err != nil {
				return nil, err
			}
			for _, imp := range imports {
				if !coveredBy(PinInputPaths("devops"), imp) {
					gaps = append(gaps, importGap{Dir: imp, Importer: v.dir})
				}
				queue = append(queue, visit{dir: imp})
			}
		}
		if sources == 0 {
			return nil, fmt.Errorf("pin-drift guard: devops directory %s has no .go files at main: renamed or removed?", v.dir)
		}
	}
	return gaps, nil
}

// coveredBy reports whether dir is one of paths or lies under one.
func coveredBy(paths []string, dir string) bool {
	for _, p := range paths {
		if dir == p || strings.HasPrefix(dir, p+"/") {
			return true
		}
	}
	return false
}
