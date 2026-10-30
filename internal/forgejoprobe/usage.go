package forgejoprobe

import (
	"fmt"
	"time"
)

// Usage is the text -h prints. Every default it names is read from the constant
// that holds it, so a default the text no longer names fails the help test.
var Usage = fmt.Sprintf(`Usage: forgejo-probe <rig> --repo OWNER/NAME [options]
  --api-url URL          API root (default $FORGEJO_API_URL,
                         else %s)
  --web-url URL          Forgejo web root, the base of the git URL (default
                         $FORGEJO_URL, else --api-url less /api/v1)
  --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
                         $FORGEJO_ADMIN_ENV, else no file is read)
  --repo OWNER/NAME      the rig's Forgejo repository (required)
  --town-root DIR        town root holding <rig>, whose repository the probe
                         pushes from (default $GT_TOWN_ROOT, else $HOME/gt)
  --github-url URL       repository to clone the copy from (default
                         https://github.com/OWNER/NAME.git)
  --git-url URL          Forgejo git remote to push to (default
                         --web-url/OWNER/NAME.git)
  --main-branch NAME     branch the copy is taken from (default: %s)
  --timeout SECONDS      how long to wait for the gate (default: %d)
  --poll-interval SEC    wait between status reads (default: %d)
  --dry-run              read but never write: no push, no branch deletion
  -h, --help             this text

Exit codes:
  0    help, or a green probe whose record was written
  1    the run failed, or the probe was red
  2    a refused command line
  3    green, but the rig's hooks were not exercised, so no record was written
  130  interrupted by SIGINT or SIGTERM before a verdict: the probe branch, the
       worktree and any stale record are cleaned up, and no record is written
`,
	DefaultAPIURL,
	DefaultMainBranch,
	int64(DefaultTimeout/time.Second),
	int64(DefaultPollInterval/time.Second),
)
