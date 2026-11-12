# OpenShell gateway

The OpenShell gateway is the long-lived service behind the `openshell` session backend: polecat seats whose `backend` is `openshell` run in sandboxes the gateway creates, and the daemon reaches them through it. The Mac gateway is retired: sandbox seats now run on the k8s gateway, and a sandbox profile with `driver: kubernetes` runs there (see [Sandbox profile driver](#sandbox-profile-driver)). The Mac gateway was the user launchd agent `com.gastown.openshell-gateway`; the Install, Restart and Regenerate certificates sections below apply only to a Mac that still runs that agent.

## Install

gt no longer installs this agent: the installer's gateway step was deleted (gt-a2uug4.18), so the plist and its launchd load are managed outside gt. The commands below operate an agent that is already installed on the Mac.

Three things must exist first; nothing creates any of them:

- the `openshell-gateway` binary at `/opt/homebrew/bin` (the brew formula);
- `~/.config/openshell/gateway.toml`, which the operator writes. Without it nothing starts the agent, so a machine with no OpenShell seats is unaffected;
- the log directory `~/gt/.runtime/openshell`, which the operator creates with `mkdir -p -m 700 ~/gt/.runtime/openshell`. The unit writes `gateway.log` there, and the directory is private because the gateway holds provider credentials.

To see what an install would do without building or changing anything:

```bash
make gt-installer && ./gt-installer --dry-run
```

The unit sets `OPENSHELL_TELEMETRY_ENABLED=false` and `OPENSHELL_LOCAL_TLS_DIR`, keeps the gateway alive (`KeepAlive`), and logs to `~/gt/.runtime/openshell/gateway.log`.

### gateway.toml settings gt needs

gt's sandbox seats need this key, in the `[openshell.drivers.docker]` table of `gateway.toml`:

```toml
[openshell.drivers.docker]
allow_driver_config = true
```

With the key absent every `sandbox create` is refused, so a seat never starts: gt reports `creating sandbox <name>: openshell sandbox create:` and the gateway's message after it. The gateway reads `gateway.toml` at start, so restart it after editing; the [Restart](#restart) section has the command.

### Sandbox profile driver

A sandbox profile's `driver` field names the compute driver its gateway runs. It takes `docker` or `kubernetes`, and `docker` is the default when the field is unset.

- A `kubernetes` profile must not set `cache_volume`. Validation refuses the pair, because a kubernetes gateway has no Docker volumes.
- A `kubernetes` profile creates no Docker cache volume and sends no `docker.mounts` config to the gateway.
- A `docker` profile, or one with no `driver`, keeps the Docker cache volume when it names a `cache_volume`.
- Any other value is refused at validation: `want docker or kubernetes`.
- A `kubernetes` profile may set `kubernetes_cache_claim` to a shared PersistentVolumeClaim, which the driver mounts at `/tmp/gt-cache` as the agent container's only mount. `/sandbox` is the sandbox's workspace PVC, and any mount under it replaces that PVC, so the pod loses its workspace.
- A `kubernetes` profile may also set `node_selector`, `tolerations`, `priority_class`, `runtime_class`, `requests` and `limits`. gt sends them to the gateway as the create call's driver config, so the gateway needs `allow_driver_config = true` in `[openshell.drivers.kubernetes]` as well; `deploy/k8s/values/kind.yaml` sets the chart's `server.drivers.kubernetes.allowDriverConfig` for the town cluster. A `docker` profile that sets any of the six is refused at validation, and a `kubernetes` profile that sets none sends no driver config.

## Restart

An install never restarts a running gateway, and neither does a changed template: a restart drops the live sessions of every sandbox the gateway serves. When the install rewrites the unit of a loaded agent it logs the command to apply it. Restart by kickstart, at a moment no polecat is mid-task:

```bash
launchctl kickstart -k gui/$(id -u)/com.gastown.openshell-gateway
```

If the agent is not loaded at all, bootstrap it:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gastown.openshell-gateway.plist
```

To remove it, `launchctl bootout gui/$(id -u)/com.gastown.openshell-gateway` and delete the plist.

## Where state lives

| Path | Holds |
|---|---|
| `~/.config/openshell/gateway.toml` | gateway configuration, operator-owned |
| `~/.local/state/openshell/` | the gateway's SQLite database and the provider credentials |
| `~/.local/state/openshell/tls/` | the local TLS material (`OPENSHELL_LOCAL_TLS_DIR`) |
| `~/gt/.runtime/openshell/gateway.log` | stdout and stderr of the gateway |

A lost `~/.local/state/openshell/` means the providers are gone; re-register them from the two key files.

## Regenerate certificates

Regenerate when `openshell status` fails with a certificate or handshake error, or after the TLS directory was deleted. The gateway writes the material on start, and the `openshell` CLI reads the same directory, so both sides pick up the new set together.

```bash
launchctl bootout gui/$(id -u)/com.gastown.openshell-gateway
mv ~/.local/state/openshell/tls ~/.local/state/openshell/tls.old
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gastown.openshell-gateway.plist
openshell status
```

`openshell status` reporting a connected gateway means the new set works; delete `tls.old` then. If it does not connect, `~/gt/.runtime/openshell/gateway.log` names the reason, and moving `tls.old` back restores the previous set.

## What a sandbox polecat cannot do

A sandbox seat runs in a container the gateway creates, not on the Mac. Before you hand it work a host polecat could do, check the task against these differences; each names the file that decides it.

- **The network is an allowlist.** `deploy/polecat/policy.yaml` denies egress by default; a URL it does not list fails.
- **Only the worktree, /tmp and a few device paths are writable.** `filesystem_policy` in the same file decides which paths a sandbox may write. The image no longer seeds the Go caches; `GOCACHE` and `GOMODCACHE` default to `/tmp/gocache` and `/tmp/gomodcache`, so a module the image lacks is fetched into the sandbox's own layer and the image is unchanged (gt-pvudz). A sandbox with a kubernetes cache claim points both at the claim's seeded directories instead (gt-nposw).
- **The environment is the sandbox's, not the host's.** `contract` in `internal/session/backend_openshell.go` sets what gt and bd read, and drops the host's credentials.
- **Git authenticates through a helper, not a stored token.** `internal/session/sandbox_runtime.go` writes the credential helper, and `cloneScript` in `internal/session/backend_openshell.go` makes the clone.
- **Commands that print the environment or a secret are refused.** The guard in `internal/cmd/tap_guard_sandbox_secrets.go` blocks them inside a sandbox only.
- **The agent's tmux is invisible to the host's.** The sandbox runs its own tmux server; gt reaches it through `openshell sandbox exec` (see `internal/session/sandbox_registry.go`).
