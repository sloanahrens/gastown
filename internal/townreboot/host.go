package townreboot

import (
	"context"
	"fmt"
	"strings"
)

// Runner runs one command and returns its standard output. An error means the
// command did not run or exited non-zero, and it must not carry the command's
// output, which can hold a secret.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// databaseContainers are the three containers the reboot leaves stopped; they
// hold every bead, Forgejo's data and history-man's index (gt-k3bf8x).
var databaseContainers = []string{"gt-dolt", "hm-postgres", "forgejo-db-1"}

// databasesFix starts the three containers, the operations doc's repair for a
// stopped database (gt-k3bf8x).
const databasesFix = "docker --context desktop-linux start gt-dolt hm-postgres forgejo-db-1"

// DatabaseCheck reads the running containers over run and passes when all three
// databases are listed.
func DatabaseCheck(run Runner) Check {
	return Check{
		Name: "databases",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-k3bf8x).
				return Result{Detail: "no runner configured"}
			}
			out, err := run(ctx, "docker", "--context", "desktop-linux", "ps", "--format", "{{.Names}}")
			if err != nil {
				// The container list is unknown, so the row says what failed
				// rather than reading an empty list as no containers running.
				return Result{Detail: fmt.Sprintf("list containers: %v", err)}
			}
			running := containerNameSet(string(out))
			var missing []string
			for _, name := range databaseContainers {
				if !running[name] {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				return Result{Detail: "not running: " + strings.Join(missing, ", "), Fix: databasesFix}
			}
			return Result{OK: true, Detail: strings.Join(databaseContainers, ", ") + " running"}
		},
	}
}

// containerNameSet is the exact name on every line of the docker ps output, so
// a container whose name merely contains a database's name is not counted.
func containerNameSet(out string) map[string]bool {
	names := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names[name] = true
		}
	}
	return names
}
