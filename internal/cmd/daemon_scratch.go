package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
)

// scratchFlags are the values `gt daemon scratch` parsed. The scratch project is
// described entirely by them: the scratch town has no settings before its first
// start installs it, so nothing is read from a town's settings.
type scratchFlags struct {
	liveRoot    string // the live town's root, read only to refuse a scratch root under or around it
	root        string // the scratch town's root, bind-mounted into the container
	composeFile string // where the compose file is written, outside root
	image       string // gt-daemon:<rev>
	project     string
	container   string
	omTokenFile string // optional: the token file's path inside the container
	forge       forgePorts
}

// forgePorts are the scratch forge's two host ports as the flags gave them:
// given both, the throwaway forge is on, given neither it is off, and one alone
// is refused. The ports themselves go to the template's own validation, so what
// is recorded here is only whether a forge was asked for.
type forgePorts struct {
	httpPort, sshPort int
	httpSet, sshSet   bool
}

// scratchDeps is what the command takes from the host: how commands run, the
// environment they start from, how a path is looked at, and where output goes.
// A test supplies fakes, so no unit test starts Docker or touches a town.
type scratchDeps struct {
	run     daemonimage.Runner
	environ []string
	out     io.Writer
	stat    func(path string) (os.FileInfo, error)
	lstat   func(path string) (os.FileInfo, error)
	// readFile reads the live town's settings file; readPatrol reads its daemon
	// settings through the config accessors; evalSymlinks resolves a path. All
	// three only read, and only the live town's settings.
	readFile     func(path string) ([]byte, error)
	readPatrol   func(townRoot string) (*config.DaemonPatrolConfig, error)
	evalSymlinks func(path string) (string, error)
}

// realScratchDeps is the host's.
func realScratchDeps(out io.Writer) scratchDeps {
	return scratchDeps{
		run: execImageRunner, environ: os.Environ(), out: out, stat: os.Stat, lstat: os.Lstat,
		readFile: os.ReadFile, readPatrol: daemon.ReadPatrolConfig, evalSymlinks: filepath.EvalSymlinks,
	}
}

// newDaemonScratchCmd builds `gt daemon scratch` with its three subcommands over
// run, which receives the subcommand's name and the flags it parsed.
func newDaemonScratchCmd(run func(ctx context.Context, action string, out io.Writer, f scratchFlags) error) *cobra.Command {
	var f scratchFlags
	parent := &cobra.Command{
		Use:   "scratch",
		Short: "Render, start and remove the daemon container's scratch project",
		Long: `The scratch project is the daemon container's rehearsal on a throwaway town:
its own project name, container, Dolt server, networks and volumes, and a town
root that is neither the live town nor anything around it.

The scratch town has no settings before its first start installs it, so these
commands take everything from flags. They run from any directory that is neither
the live town nor the scratch town. --live-root names the live town and is read
only to refuse a scratch root under or around it; nothing is run or written in
the live town.`,
		RunE: requireSubcommand,
	}
	pf := parent.PersistentFlags()
	pf.StringVar(&f.liveRoot, "live-root", "", "absolute path of the live town (read only, to refuse a scratch root under or around it)")
	pf.StringVar(&f.root, "root", "", "absolute scratch town root, holding only its .gt-scratch-town marker before the first start")
	pf.StringVar(&f.composeFile, "compose-file", "", "absolute path of the compose file, outside --root")
	pf.StringVar(&f.project, "project", "", "scratch compose project (not the live project)")
	pf.StringVar(&f.container, "container", "", "scratch daemon container name (not the live container)")
	for _, action := range []struct{ name, short string }{
		{"render", "Print the scratch compose file and write nothing"},
		{"up", "Write the scratch compose file and start the project"},
		{"down", "Remove the scratch project's containers and volumes"},
	} {
		sub := &cobra.Command{
			Use:   action.name,
			Short: action.short,
			Args:  cobra.NoArgs,
		}
		name := action.name
		sub.RunE = func(cmd *cobra.Command, _ []string) error {
			f.forge.httpSet = cmd.Flags().Changed("forge-http-port")
			f.forge.sshSet = cmd.Flags().Changed("forge-ssh-port")
			return run(cmd.Context(), name, cmd.OutOrStdout(), f)
		}
		if name != "down" {
			sub.Flags().StringVar(&f.image, "image", "", "daemon image tag, gt-daemon:<rev>")
			sub.Flags().StringVar(&f.omTokenFile, config.OMTokenFileFlag, "", "path of the om token file inside the container, passed to the daemon's command line")
			sub.Flags().IntVar(&f.forge.httpPort, "forge-http-port", 0, "host port of the scratch forge's http; given with --forge-ssh-port it adds the throwaway forge to the project")
			sub.Flags().IntVar(&f.forge.sshPort, "forge-ssh-port", 0, "host port of the scratch forge's ssh; given with --forge-http-port it adds the throwaway forge to the project")
		}
		parent.AddCommand(sub)
	}
	return parent
}

var daemonScratchCmd = newDaemonScratchCmd(func(ctx context.Context, action string, out io.Writer, f scratchFlags) error {
	return runDaemonScratch(ctx, action, f, realScratchDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonScratchCmd)
}

// runDaemonScratch runs one of render, up and down. Every refusal comes before
// any file is written or command run.
func runDaemonScratch(ctx context.Context, action string, f scratchFlags, d scratchDeps) error {
	switch action {
	case "render":
		spec, err := scratchSpecFor(f, d)
		if err != nil {
			return err
		}
		content, err := templates.RenderDaemonCompose(spec)
		if err != nil {
			return err
		}
		_, err = d.out.Write(content)
		return err
	case "up":
		spec, err := scratchSpecFor(f, d)
		if err != nil {
			return err
		}
		if err := checkScratchRoot(f.root, d); err != nil {
			return err
		}
		start := append([]string{"up", "-d", "daemon"}, templates.ScratchForgeServices(spec)...)
		sup := containerDeploySupervisor{start: func() ([]string, error) { return scratchComposeArgv(f, start...), nil }, container: f.container}
		deployer := daemonimage.Deployer{Run: d.run, Environ: d.environ}
		return deployer.Deploy(ctx, sup, f.composeFile, spec)
	case "down":
		// down removes volumes, so it is checked against the live town as hard as
		// up is: the same names guard, with the live town's settings read.
		for _, req := range []struct{ flag, value string }{{"--live-root", f.liveRoot}, {"--compose-file", f.composeFile}} {
			if strings.TrimSpace(req.value) == "" {
				return fmt.Errorf("%s is required", req.flag)
			}
		}
		if err := scratchGuards(f, d); err != nil {
			return err
		}
		if info, err := d.stat(f.composeFile); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("--compose-file %q is not a file: down removes the project that file describes, and there is none to remove", f.composeFile)
		}
		return d.run(ctx, filepath.Dir(f.composeFile), daemonimage.DockerEnv(d.environ), scratchComposeArgv(f, "down", "-v"))
	}
	return fmt.Errorf("unknown scratch action %q", action)
}

// scratchComposeArgv is a compose command on the scratch project: the explicit
// context, the project and the file, then the subcommand.
func scratchComposeArgv(f scratchFlags, sub ...string) []string {
	argv := []string{"docker", "--context", daemonimage.DockerContext, "compose", "-p", f.project, "-f", f.composeFile}
	return append(argv, sub...)
}

// scratchSpecFor validates the flags and builds the scratch render's spec from
// them. The refusals here are the command's own; the render adds its own (the
// live identity, the live root through a link).
func scratchSpecFor(f scratchFlags, d scratchDeps) (templates.ComposeSpec, error) {
	for _, req := range []struct{ flag, value string }{
		{"--live-root", f.liveRoot}, {"--root", f.root}, {"--compose-file", f.composeFile}, {"--image", f.image},
	} {
		if strings.TrimSpace(req.value) == "" {
			return templates.ComposeSpec{}, fmt.Errorf("%s is required", req.flag)
		}
	}
	if err := requireAbs("--root", f.root); err != nil {
		return templates.ComposeSpec{}, err
	}
	if err := scratchGuards(f, d); err != nil {
		return templates.ComposeSpec{}, err
	}
	if f.omTokenFile != "" {
		if err := requireAbs("--"+config.OMTokenFileFlag, f.omTokenFile); err != nil {
			return templates.ComposeSpec{}, err
		}
	}
	if pathWithin(f.composeFile, f.root) {
		return templates.ComposeSpec{}, fmt.Errorf("--compose-file %q is inside --root %q: the container installs the scratch town only into a directory that holds nothing but its marker, so the compose file lives outside it", f.composeFile, f.root)
	}
	forge, err := scratchForge(f.forge)
	if err != nil {
		return templates.ComposeSpec{}, err
	}
	return templates.ComposeSpec{
		Project:              f.project,
		ContainerName:        f.container,
		Image:                f.image,
		TownRoot:             f.root,
		StopGrace:            templates.MinimumStopGrace,
		Scratch:              true,
		ScratchDoltImage:     templates.DoltServerImage,
		LiveTownRoot:         f.liveRoot,
		OMTokenFile:          f.omTokenFile,
		ScratchForge:         forge.on,
		ScratchForgeHTTPPort: forge.httpPort,
		ScratchForgeSSHPort:  forge.sshPort,
	}, nil
}

// scratchForgeFields is whether the scratch forge is on and, when it is, the
// two host ports it publishes.
type scratchForgeFields struct {
	on                bool
	httpPort, sshPort int
}

// scratchForge decides whether the flags ask for the scratch forge: both ports
// turn it on with those ports, neither leaves it off, and one alone is refused,
// because a forge named on half a port pair would publish one of its services
// and leave the other without a host port. The ports are passed on as given, so
// the template's own checkScratchForge refuses a live port, an equal pair or a
// value that is not a port, rather than a second copy of that check here.
func scratchForge(p forgePorts) (scratchForgeFields, error) {
	switch {
	case p.httpSet && p.sshSet:
		return scratchForgeFields{on: true, httpPort: p.httpPort, sshPort: p.sshPort}, nil
	case p.httpSet:
		return scratchForgeFields{}, fmt.Errorf("--forge-http-port %d is given without --forge-ssh-port: the scratch forge takes both its host ports, or neither for no forge", p.httpPort)
	case p.sshSet:
		return scratchForgeFields{}, fmt.Errorf("--forge-ssh-port %d is given without --forge-http-port: the scratch forge takes both its host ports, or neither for no forge", p.sshPort)
	}
	return scratchForgeFields{}, nil
}

// validateScratchNames refuses a project or container name the scratch project
// may not use: either one unset, one set without the other, one of an invalid
// shape, or one of the live town's defaults.
func validateScratchNames(f scratchFlags) error {
	if strings.TrimSpace(f.project) == "" || strings.TrimSpace(f.container) == "" {
		return fmt.Errorf("--project and --container are both required: a scratch name left unset could fall back to the live town's")
	}
	names := &config.DaemonPatrolConfig{ContainerNameSetting: f.container, ComposeProjectSetting: f.project}
	if err := names.ValidateContainerNames(); err != nil {
		return err
	}
	switch {
	case f.container == config.DefaultContainerName:
		return fmt.Errorf("--container %q is the live daemon's container name", f.container)
	case f.project == config.DefaultComposeProject:
		return fmt.Errorf("--project %q is the live compose project", f.project)
	}
	return nil
}

// checkScratchRoot is the preflight of `up`: the scratch root exists as a
// directory and holds the scratch marker, so a missing or wrong root is refused
// before the project's containers start rather than by the entrypoint after.
func checkScratchRoot(root string, d scratchDeps) error {
	if info, err := d.stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("--root %q is not an existing directory: create it and its %s marker first", root, config.ScratchTownMarker)
	}
	if info, err := d.lstat(filepath.Join(root, config.ScratchTownMarker)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("--root %q holds no %s marker: the entrypoint installs a scratch town only into a directory that does", root, config.ScratchTownMarker)
	}
	return nil
}

// requireAbs refuses a path that is not absolute: docker resolves a relative one
// against the directory it runs in, not this command's.
func requireAbs(flag, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q is not absolute", flag, path)
	}
	return nil
}

// pathWithin reports whether path is root or lies under it, by their cleaned
// absolute spellings.
func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// scratchGuards is the refusal helper render, up and down share, so none of them
// can act on a project or a file the live town could be reached through: the
// scratch names are valid and are neither the live defaults nor the names the
// live town's own settings give, the live root is an absolute town, and the
// compose file is not inside it.
func scratchGuards(f scratchFlags, d scratchDeps) error {
	if err := validateScratchNames(f); err != nil {
		return err
	}
	for _, p := range []struct{ flag, value string }{{"--live-root", f.liveRoot}, {"--compose-file", f.composeFile}} {
		if err := requireAbs(p.flag, p.value); err != nil {
			return err
		}
	}
	info, err := d.lstat(filepath.Join(f.liveRoot, "mayor", "town.json"))
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("--live-root %q is not a town (no regular mayor/town.json): it is read only to refuse a scratch root under or around it, so it must name the live town", f.liveRoot)
	}
	if err := refuseLiveConfiguredNames(f, d); err != nil {
		return err
	}
	return refuseComposeFileInLiveRoot(f, d)
}

// refuseLiveConfiguredNames refuses a scratch project or container that equals
// the name the live town's own settings give it. The compiled-in defaults are
// refused by validateScratchNames; a live town may override them, and down
// removes volumes, so this fails closed: a settings file that is present and
// cannot be read or parsed refuses, never "no override found". A missing
// settings file means the defaults only.
//
// The raw settings/config.json is read as well as the daemon settings through
// the config accessors, because the accessors treat a settings file that does
// not parse as absent and so would report no override for a broken one.
func refuseLiveConfiguredNames(f scratchFlags, d scratchDeps) error {
	settings := filepath.Join(f.liveRoot, "settings", "config.json")
	data, err := d.readFile(settings)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("the live town's settings %s cannot be read, so its container and project names are unknown and the scratch names cannot be checked against them: %w", settings, err)
	default:
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			return fmt.Errorf("the live town's settings %s do not parse, so its container and project names are unknown and the scratch names cannot be checked against them", settings)
		}
	}
	live, err := d.readPatrol(f.liveRoot)
	if err != nil {
		return fmt.Errorf("the live town's daemon settings cannot be read, so its container and project names are unknown and the scratch names cannot be checked against them: %w", err)
	}
	switch {
	case f.container == live.ContainerName():
		return fmt.Errorf("--container %q is the live town's container name", f.container)
	case f.project == live.ComposeProject():
		return fmt.Errorf("--project %q is the live town's compose project", f.project)
	}
	return nil
}

// refuseComposeFileInLiveRoot refuses a compose file that resolves inside the
// live town root, or a live root that lies inside the compose file's path, on
// resolved paths: a link into the live town names it under another spelling.
func refuseComposeFileInLiveRoot(f scratchFlags, d scratchDeps) error {
	live, err := d.evalSymlinks(f.liveRoot)
	if err != nil {
		return fmt.Errorf("--live-root %q cannot be resolved: %w", f.liveRoot, err)
	}
	compose, err := resolveThroughExisting(f.composeFile, d.evalSymlinks)
	if err != nil {
		return fmt.Errorf("--compose-file %q cannot be resolved: %w", f.composeFile, err)
	}
	if pathWithin(compose, live) || pathWithin(live, compose) {
		return fmt.Errorf("--compose-file %q resolves inside the live town root %q: the scratch project's files stay outside the live town", f.composeFile, f.liveRoot)
	}
	return nil
}

// resolveThroughExisting resolves path's symbolic links when the file need not
// exist yet: the nearest existing ancestor is resolved and the rest appended.
func resolveThroughExisting(path string, eval func(string) (string, error)) (string, error) {
	var rest []string
	for p := filepath.Clean(path); ; {
		resolved, err := eval(p)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return "", err
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}
