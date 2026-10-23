#!/usr/bin/env bash
# Imports the OpenShell profiles a polecat sandbox needs and creates the two
# providers they name, from the key files under ~/.config/gt (claude-q3f.9).
# Run it through `make openshell-providers`.
#
# Nothing secret reaches the terminal: xtrace is off for the whole run, a key
# file is read inside a subshell, the credential travels back to provider
# create by name (so it never enters argv or the output of a command), and
# whatever provider create prints passes a length mask first.
set -euo pipefail
set +x

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROFILES="$ROOT/deploy/polecat/profiles"
CONFIG_DIR="${GT_CONFIG_DIR:-$HOME/.config/gt}"
# Any 24+ character run of token characters in tool output is a credential.
MASK='s/[A-Za-z0-9_-]{24,}/***/g'

masked() { sed -E "$MASK"; }

if ! command -v openshell >/dev/null 2>&1; then
  echo "openshell-providers: openshell is not on PATH (brew install openshell)" >&2
  exit 1
fi

# A provider that already exists is left as it is: provider list is the state,
# the key files are only read when a create is actually needed. The list is
# captured whole, so grep -q cannot close the pipe on a live openshell and turn
# a match into a SIGPIPE failure under pipefail.
provider_exists() {
  local listed
  listed="$(openshell provider list 2>/dev/null || true)"
  grep -q -F -- "$1" <<<"$listed"
}

import_profiles() {
  local file
  local profiles=()
  shopt -s nullglob
  profiles=("$PROFILES"/*.yaml)
  shopt -u nullglob
  if [[ ${#profiles[@]} -eq 0 ]]; then
    echo "openshell-providers: no profiles under ${PROFILES#"$ROOT"/}, nothing to import"
    return 0
  fi
  for file in "${profiles[@]}"; do
    openshell profile lint -f "$file"
    openshell profile import -f "$file" --global
  done
}

# create_provider NAME TYPE FILE KEY_CREDENTIAL
# FILE defines KEY; CREDENTIAL is the env var the profile rewrites, which
# provider create takes as a name and reads from this subshell's environment.
create_provider() {
  local name=$1 type=$2 file=$3 key=$4 credential=$5
  if provider_exists "$name"; then
    echo "openshell-providers: provider $name exists"
    return 0
  fi
  if [[ ! -r "$file" ]]; then
    echo "openshell-providers: $file is unreadable, so provider $name cannot be created" >&2
    exit 1
  fi
  (
    # shellcheck disable=SC1090,SC1091 # the operator's key file, outside the repo
    . "$file"
    if [[ -z "${!key:-}" ]]; then
      echo "openshell-providers: $file does not set $key" >&2
      exit 1
    fi
    export "$credential=${!key}"
    openshell provider create --name "$name" --type "$type" --credential "$credential" | masked
  )
}

import_profiles
create_provider deepseek deepseek-claude "$CONFIG_DIR/deepseek.env" DEEPSEEK_API_KEY ANTHROPIC_AUTH_TOKEN
create_provider forgejo-polecat forgejo-polecat "$CONFIG_DIR/forgejo-polecat.env" FORGEJO_TOKEN FORGEJO_TOKEN
