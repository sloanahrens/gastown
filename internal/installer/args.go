package installer

import (
	"errors"
	"fmt"
)

// Config is one run's arguments, as scripts/install-gt.sh documents them.
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
}

// Parse reads the arguments install-gt.sh accepts. On a bad argument it returns
// the config read so far, because that is the commit the run's result line
// still names: the script's trap reports the SHA it had reached.
func Parse(args []string) (Config, error) {
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
