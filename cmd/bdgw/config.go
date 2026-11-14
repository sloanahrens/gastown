package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// The container's paths: /ws is the workspace tree the gateway owns and
// /usr/local/bin/bd is the real bd the gateway runs. A flag overrides each, so a
// test or a Mac-side run names its own.
const (
	defaultWorkspaceRoot = "/ws"
	defaultBinary        = "/usr/local/bin/bd"
	defaultAddr          = ":7440"
)

// config is the gateway's fixed settings, shared by all three modes. The names
// are this command's; the design names only the values (the design's "The
// workspace", "The compose unit" and "Environment allowlist").
type config struct {
	bd               string
	workspaceRoot    string
	home             string
	searchPath       string
	doltHost         string
	doltPort         string
	trackerURL       string
	trackerTokenFile string
	verbsPath        string
	logDir           string
	tokensDir        string
	beadPrefixes     string
	maxConcurrent    int
}

func defaultConfig() config {
	return config{
		bd:               defaultBinary,
		workspaceRoot:    defaultWorkspaceRoot,
		home:             os.Getenv("HOME"),
		searchPath:       "/usr/local/bin:/usr/bin:/bin",
		doltHost:         "dolt",
		doltPort:         "3306",
		trackerURL:       os.Getenv("TRACKER_URL"),
		trackerTokenFile: os.Getenv("TRACKER_TOKEN_FILE"),
		beadPrefixes:     "gt,hq",
		maxConcurrent:    bdgateway.DefaultMaxConcurrent,
	}
}

// bindShared adds the flags every mode reads. A value the container supplies
// through the environment (the tracker credential, HOME) is the flag's default,
// so the environment is read once, here.
func bindShared(fs *flag.FlagSet, c *config) {
	fs.StringVar(&c.bd, "bd", c.bd, "the bd program the gateway runs")
	fs.StringVar(&c.workspaceRoot, "workspace", c.workspaceRoot, "the workspace tree's root")
	fs.StringVar(&c.home, "home", c.home, "the child's HOME")
	fs.StringVar(&c.searchPath, "path", c.searchPath, "the child's PATH")
	fs.StringVar(&c.doltHost, "dolt-host", c.doltHost, "the Dolt host the workspace resolves to")
	fs.StringVar(&c.doltPort, "dolt-port", c.doltPort, "the Dolt port the workspace resolves to")
	fs.StringVar(&c.trackerURL, "tracker-url", c.trackerURL, "the tracker endpoint, empty when the town runs none")
	fs.StringVar(&c.trackerTokenFile, "tracker-token-file", c.trackerTokenFile, "the gateway's tracker credential file")
	fs.StringVar(&c.verbsPath, "verbs", c.verbsPath, "the verbs file (deploy/bd/verbs.json)")
	fs.StringVar(&c.logDir, "log-dir", c.logDir, "the call log's directory")
	fs.StringVar(&c.tokensDir, "tokens-dir", c.tokensDir, "the token directory")
	fs.StringVar(&c.beadPrefixes, "bead-prefixes", c.beadPrefixes, "comma-separated bead id prefixes the call log may record")
	fs.IntVar(&c.maxConcurrent, "max-concurrent", c.maxConcurrent, "the most bd processes allowed at once")
}

// require fails when one of the named settings is empty, naming the flag, so a
// missing mount is a startup error rather than a default the operator never
// chose. trackerURL is allowed to be empty: the town may run no tracker.
func (c config) require(names ...string) error {
	for _, name := range names {
		var value string
		switch name {
		case "tokens-dir":
			value = c.tokensDir
		case "verbs":
			value = c.verbsPath
		case "log-dir":
			value = c.logDir
		case "workspace":
			value = c.workspaceRoot
		case "bd":
			value = c.bd
		case "home":
			value = c.home
		default:
			return fmt.Errorf("bdgw: unknown setting %q", name)
		}
		if value == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}
	return nil
}

// prefixes splits the bead id prefixes for the call log. At least one is
// required: the log refuses to start without them, because a word that looks
// like a bead id but is not one would otherwise be recorded.
func (c config) prefixes() []string {
	var out []string
	for _, p := range strings.Split(c.beadPrefixes, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// executor builds the one executor both transports run calls through.
func (c config) executor() (*bdgateway.Executor, error) {
	e, err := bdgateway.New(bdgateway.Options{
		Binary:           c.bd,
		WorkspaceRoot:    c.workspaceRoot,
		Home:             c.home,
		Path:             c.searchPath,
		DoltHost:         c.doltHost,
		DoltPort:         c.doltPort,
		TrackerURL:       c.trackerURL,
		TrackerTokenFile: c.trackerTokenFile,
		MaxConcurrent:    c.maxConcurrent,
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// callLog opens the JSONL call log in the configured directory.
func (c config) callLog(policy *bdgateway.Policy) (*bdgateway.CallLog, error) {
	if c.logDir == "" {
		return nil, errors.New("--log-dir is required")
	}
	log, err := bdgateway.NewCallLog(bdgateway.CallLogOptions{
		Dir:          c.logDir,
		Policy:       policy,
		BeadPrefixes: c.prefixes(),
	})
	if err != nil {
		return nil, err
	}
	return log, nil
}
