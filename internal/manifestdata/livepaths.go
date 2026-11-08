package manifestdata

// The live container's paths, shared by the compose writer and the cluster
// renderer so the two services mount and name the same files.

// LiveOpenShellConfigHome is the XDG_CONFIG_HOME the live container's daemon
// runs with, and LiveOpenShellConfigPath is where the host's openshell config
// directory (gateway metadata and the mTLS client certificates) is mounted
// read-only beneath it, so the container's openshell finds the gateways the way
// the host's does (gt-yj0vh). LiveConfigDirPath is where the town's config
// directory is mounted beneath it, so ~/.config/gt resolves inside the
// container (gt-gg9hv.3).
const (
	LiveOpenShellConfigHome = "/run/gt/xdg"
	LiveOpenShellConfigPath = LiveOpenShellConfigHome + "/openshell"
	LiveConfigDirPath       = LiveOpenShellConfigHome + "/gt"
)

// LiveOMTokenPath is where the live render mounts the om token file inside the
// container, and the path its command line passes to the entrypoint. It is one
// fixed path because the host file's own path is the operator's: the mount's
// target is the only place the container can name it, and the compose file then
// carries no host path on the walk to the entrypoint. It sits under /run, so the
// mount never shadows a file the image ships, and the token is on the container's
// filesystem only for as long as the read-only mount exists.
const LiveOMTokenPath = "/run/secrets/om-token" //nolint:gosec // a mount path, not a credential

// LiveOMConfigPath is where om reads the derived config beneath the container's
// XDG home, so its backend is the container's claude-exec and not a bare claude
// with no credential (gt-gg9hv.13).
const LiveOMConfigPath = LiveOpenShellConfigHome + "/om/config.json"

// LiveForgeTokenPath is the same fixed path for the forge token file the live
// render mounts read-only, and the path its command line passes to the
// entrypoint as --forge-token-file. One fixed path, so the entrypoint's own
// check on the path (a shell reads it in git's helper line) passes whatever the
// host path is, and the host path is never spelled on the command line
// (gt-rpx80).
const LiveForgeTokenPath = "/run/secrets/forge-token" //nolint:gosec // a mount path, not a credential
