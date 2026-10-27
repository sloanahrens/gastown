#!/usr/bin/env bash
# Exec shim for this plugin's Go program, which the plugin runner execs
# directly; plugin.md is the report's manual (gt-tpg0p.3).
exec "$(dirname "$0")/bin/tool-updater" "$@"
