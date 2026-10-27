package installer

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/imagebuild"
)

// ImageWorker is a detached image build's entry point: the process the
// installer starts for --no-image-first, so the build outlives the install
// (gt-tpg0p.12).
//
//	gt-installer --image-build-worker --alarm SECONDS [--lock DIR]
//	             [--gt PATH --fingerprint FP] -- PROGRAM [ARG...]
//
// It detaches before starting the build: a session of its own and none of the
// descriptors it inherited, the install lock among them (gt-xzo8a). The build
// settings arrive in the environment the installer gives it.
func ImageWorker(args []string, stderr io.Writer) int {
	return imageWorker(args, stderr, imagebuild.Detach, imagebuild.Run)
}

// imageWorker is ImageWorker with the detach and the supervised run injected,
// so a test exercises the entry point without a session or a process.
func imageWorker(args []string, stderr io.Writer, detach func(), run func(imagebuild.Config) int) int {
	cfg, err := parseImageWorker(args, stderr)
	if err != nil {
		if errors.Is(err, errImageWorkerUsage) {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	detach()
	return run(cfg)
}

// errImageWorkerUsage is a worker invocation that parsed but names no build.
var errImageWorkerUsage = errors.New("usage: " + ImageWorkerFlag +
	" --alarm SECONDS [--lock DIR] [--gt PATH --fingerprint FP] -- PROGRAM [ARG...]")

// parseImageWorker reads the worker's arguments. A flag error is reported by
// the flag set itself; the usage error is reported by the caller.
func parseImageWorker(args []string, stderr io.Writer) (imagebuild.Config, error) {
	fs := flag.NewFlagSet("gt-installer image worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	alarm := fs.Int("alarm", 0, "seconds the build may run before it is stopped")
	lock := fs.String("lock", "", "lock directory the build takes; removed after a stop that it names")
	gt := fs.String("gt", "", "gt binary that raises and clears the escalation")
	fingerprint := fs.String("fingerprint", "", "escalation fingerprint for a build that does not finish")
	if err := fs.Parse(args); err != nil {
		return imagebuild.Config{}, err
	}
	if *alarm <= 0 || fs.NArg() == 0 || (*fingerprint != "") != (*gt != "") {
		return imagebuild.Config{}, errImageWorkerUsage
	}
	return imagebuild.Config{
		Alarm:       time.Duration(*alarm) * time.Second,
		Lock:        *lock,
		GT:          *gt,
		Fingerprint: *fingerprint,
		Command:     fs.Args(),
	}, nil
}
