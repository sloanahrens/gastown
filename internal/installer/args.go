package installer

import (
	"errors"
	"fmt"
)

// Config is one run's arguments, as cmd/gt-installer documents them.
type Config struct {
	// SHA is --sha: the commit to install, "origin/main" (resolved after the
	// fetch) when the caller names none.
	SHA string
	// Source is --source: manual, rebuild-gt or post-land.
	Source string
	// SlotRole is --slot-role: the gate slot role the build runs under; empty
	// runs the build without one.
	SlotRole string
	// SlotTimeout is --slot-timeout in seconds, as given: the slot step appends
	// the "s".
	SlotTimeout string
	// DryRun is --dry-run: say what an install would do and change nothing.
	DryRun bool
	// ImageFirst is --no-image-first's inverse: build and publish the sandbox
	// image before the binary swap.
	ImageFirst bool
	// Worker says the arguments are a detached image build's worker, not an
	// install: the worker runs a build this program started for
	// --no-image-first and names no result line (gt-tpg0p.12).
	Worker bool
	// WorkerArgs is the worker's own arguments, after ImageWorkerFlag.
	WorkerArgs []string
}

// Parse reads the arguments an install accepts. On a bad argument it returns
// the config read so far, because that is the commit the run's result line
// still names: the script's trap reports the SHA it had reached.
func Parse(args []string) (Config, error) {
	// A detached image build's worker is not an install: its first argument
	// alone routes it, before any install flag is read, because its own flags
	// belong to the worker's parser. A worker token anywhere else is no more
	// an install flag than any other unknown argument (gt-tpg0p.12).
	if len(args) > 0 && args[0] == ImageWorkerFlag {
		return Config{Worker: true, WorkerArgs: append([]string(nil), args[1:]...)}, nil
	}
	cfg := Config{SHA: "origin/main", Source: "manual", SlotTimeout: "600", ImageFirst: true}
	for i := 0; i < len(args); i++ {
		var target *string
		switch args[i] {
		case "--sha":
			target = &cfg.SHA
		case "--source":
			target = &cfg.Source
		case "--slot-role":
			target = &cfg.SlotRole
		case "--slot-timeout":
			target = &cfg.SlotTimeout
		case "--dry-run":
			cfg.DryRun = true
			continue
		case "--no-image-first":
			cfg.ImageFirst = false
			continue
		default:
			return cfg, fmt.Errorf("install-gt: unknown argument: %s", args[i])
		}
		if i+1 == len(args) {
			// The script's ${2:-} assigns the empty value before `shift 2`
			// aborts, so the flag is emptied here too and only the message
			// names the flag instead of bash.
			*target = ""
			return cfg, fmt.Errorf("install-gt: %s needs a value", args[i])
		}
		i++
		*target = args[i]
	}
	switch cfg.Source {
	case "manual", "rebuild-gt", "post-land":
	default:
		return cfg, errors.New("install-gt: --source must be manual, rebuild-gt or post-land")
	}
	if cfg.SHA == "" {
		return cfg, errors.New("install-gt: --sha needs a commit")
	}
	return cfg, nil
}
