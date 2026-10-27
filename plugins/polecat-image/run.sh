#!/usr/bin/env bash
# Exec shim for this plugin's Go program, which the plugin runner execs
# directly; plugin.md defines the rebuild, publish and trim gates (gt-tpg0p.7).
exec "$(dirname "$0")/bin/polecat-image" "$@"
