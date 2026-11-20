package daemonimage

// devopsInputPaths is what the daemon image builds from the devops repository
// (tools.go, build.go): the three commands, the packages they import, the
// cloud-check file staged beside cloudcheck, and the module files. A change
// outside this list cannot reach the image (gt-f3rjtl).
var devopsInputPaths = []string{
	"cmd/reporter", "cmd/release-reporter", "cmd/cloudcheck", "internal/ledgerdb",
	"pkg/patrol", "pkg/releasebot", "ops/cloud-check.apps", "go.mod", "go.sum",
}

// PinInputPaths returns the repository paths the image builds the named pin
// from, and nil for a source it builds from its whole repository: bd, om, hm and
// any name it does not know, which then counts every commit.
func PinInputPaths(name string) []string {
	if name == "devops" {
		return devopsInputPaths
	}
	return nil
}
