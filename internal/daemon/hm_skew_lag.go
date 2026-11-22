package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// skewLag reads how far behind remote the installed commit is, in the two measures
// the planner that refreshes the installed pin files a refresh at (imagePinsFileBehind
// commits, imagePinsFileAge): the commits `git rev-list --count installed..remote` counts, and the committer time
// of remote minus that of installed. Both revisions are held to a commit id before they
// reach git, and the calls run through omSkewGit as the diff does. An output that does
// not parse is an error naming the git call, never a zero reading (gt-c88j63). The name
// is generic so the om skew check can call the same reading later.
func (d *Daemon) skewLag(src, installed, remote string) (commits int, age time.Duration, err error) {
	if err := omSkewCommitRevs(installed, remote); err != nil {
		return 0, 0, err
	}
	out, err := d.omSkewGit("-C", src, "rev-list", "--count", installed+".."+remote)
	if err != nil {
		return 0, 0, err
	}
	commits, err = strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, 0, fmt.Errorf("git rev-list --count %s..%s printed %q, not a count", installed, remote, strings.TrimSpace(out))
	}
	installedAt, err := d.skewCommitTime(src, installed)
	if err != nil {
		return 0, 0, err
	}
	remoteAt, err := d.skewCommitTime(src, remote)
	if err != nil {
		return 0, 0, err
	}
	return commits, remoteAt.Sub(installedAt), nil
}

// skewCommitTime is the committer time of rev in the clone src: `git show -s
// --format=%ct <rev> --` prints it in seconds since the epoch.
func (d *Daemon) skewCommitTime(src, rev string) (time.Time, error) {
	out, err := d.omSkewGit("-C", src, "show", "-s", "--format=%ct", rev, "--")
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("git show -s --format=%%ct %s printed %q, not a time", rev, strings.TrimSpace(out))
	}
	return time.Unix(secs, 0), nil
}

// hmSkewPastThresholds reports whether the changed paths can change the hm binary
// and the installed hm is imagePinsFileBehind commits or imagePinsFileAge behind
// remote, the planner's two measures. The lag is read only for a diff that can change
// the binary; below both measures the reading is not behind. A lag that cannot be
// measured is an error and so no skew (gt-g7tfxp).
func (d *Daemon) hmSkewPastThresholds(src, installed, remote string, changed []string, minCommits int, minAge time.Duration) (bool, error) {
	reaches, err := d.hmSkewReachesBinary(src, remote, changed)
	if err != nil || !reaches || (minCommits == 0 && minAge == 0) {
		return reaches, err
	}
	commits, age, err := d.skewLag(src, installed, remote)
	if err != nil {
		return false, fmt.Errorf("cannot measure how far behind the installed hm is: %w", err)
	}
	return commits >= minCommits || age >= minAge, nil
}
