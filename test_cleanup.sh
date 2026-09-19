#!/bin/bash
set -euo pipefail
TMP=""
cleanup() { [[ -n "$TMP" && -d "$TMP" ]] && rm -rf "$TMP"; }
cleanup
echo "cleanup done"
TMP="/tmp/test"
cleanup
echo "cleanup with dir"
