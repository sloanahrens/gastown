#!/usr/bin/env bash
# Exec shim for this plugin's Go program, which the plugin runner execs
# directly; plugin.md defines the promotion gate (gt-tpg0p.4).
exec "$(dirname "$0")/bin/app-promote" "$@"
