# OpenShell gateway

The OpenShell gateway is the long-lived service behind the `openshell` session backend: polecat seats whose `backend` is `openshell` run in sandboxes the gateway creates, and the daemon reaches them through it. On macOS it runs as the user launchd agent `com.gastown.openshell-gateway`. Read this before installing, restarting or repairing the gateway.

## Install

The agent is installed by `scripts/install-gt.sh`, the same run that installs `gt` (`make install`, the rebuild-gt plugin). The install writes `~/Library/LaunchAgents/com.gastown.openshell-gateway.plist` from `scripts/launchd/com.gastown.openshell-gateway.plist.tmpl`, with `$HOME` filled in, and loads it with `launchctl bootstrap gui/$(id -u)` when launchd has not loaded it yet.

Two things must exist first; the install does not create either:

- the `openshell-gateway` binary at `/opt/homebrew/bin` (the brew formula);
- `~/.config/openshell/gateway.toml`, which the operator writes. Without it the install skips the agent, so a machine with no OpenShell seats is unaffected.

To see what an install would do without building or changing anything:

```bash
scripts/install-gt.sh --dry-run
```

It prints `gateway: skipped, no <gateway.toml>` when the file is missing. Otherwise it prints `would write <plist>` when the unit differs from the template, and `would run: launchctl bootstrap gui/<uid> <plist>` when launchd has not loaded the unit.

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

## Restart

An install never restarts a running gateway, and neither does a changed template: a restart drops the live sessions of every sandbox the gateway serves. When the install rewrites the unit of a loaded agent it logs the command to apply it. Restart by kickstart, at a moment no polecat is mid-task:

```bash
launchctl kickstart -k gui/$(id -u)/com.gastown.openshell-gateway
```

If the agent is not loaded at all, bootstrap it:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gastown.openshell-gateway.plist
```

To remove it, `launchctl bootout gui/$(id -u)/com.gastown.openshell-gateway` and delete the plist; the next install writes it again while `gateway.toml` exists.

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

## Doctor

`gt doctor` runs the `openshell-gateway` check on the sandbox profiles the openshell seats use. It warns, and names each missing piece, when the gateway does not report connected, a provider a profile names is not registered, a profile image is not present, or (on macOS) the launchd agent is not loaded. It also warns when the town settings or `polecat_pool.seats` cannot be read, and when the docker CLI is not on PATH: the image check cannot run then, so the warning says the images are unverified. A town with no openshell seats passes without running any command.

## What a sandbox polecat cannot do

A sandbox seat runs in a container the gateway creates, not on the Mac. Before you hand it work a host polecat could do, check the task against these differences; each names the file that decides it.

- **The network is an allowlist.** `deploy/polecat/policy.yaml` denies egress by default; a URL it does not list fails.
- **Only the worktree and a few device paths are writable.** The same file's `filesystem_policy` decides which paths a sandbox may write.
- **The environment is the sandbox's, not the host's.** `contract` in `internal/session/backend_openshell.go` sets what gt and bd read, and drops the host's credentials.
- **Git authenticates through a helper, not a stored token.** `internal/session/sandbox_runtime.go` writes the credential helper, and `cloneScript` in `internal/session/backend_openshell.go` makes the clone.
- **Commands that print the environment or a secret are refused.** The guard in `internal/cmd/tap_guard_sandbox_secrets.go` blocks them inside a sandbox only.
- **The agent's tmux is invisible to the host's.** The sandbox runs its own tmux server; gt reaches it through `openshell sandbox exec` (see `internal/session/sandbox_registry.go`).
