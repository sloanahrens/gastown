#!/usr/bin/env bash
# Imports the OpenShell profiles a polecat sandbox needs and creates the two
# providers they name, from the key files under ~/.config/gt (claude-q3f.9).
# Run it through `make openshell-providers`.
#
# A profile or provider the gateway already lists is left alone, so a rerun of
# the target changes nothing instead of failing on the first thing it meets.
#
# Nothing secret reaches the terminal: xtrace is off for the whole run, a key
# file is read as data (never sourced), the credential travels back to provider
# create by name (so it never enters argv or the output of a command), and
# whatever provider create prints has every occurrence of the secret itself
# replaced before it is shown.
set -euo pipefail
set +x

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROFILES="$ROOT/deploy/polecat/profiles"
CONFIG_DIR="${GT_CONFIG_DIR:-$HOME/.config/gt}"

if ! command -v openshell >/dev/null 2>&1; then
  echo "openshell-providers: openshell is not on PATH (brew install openshell)" >&2
  exit 1
fi

# A provider that already exists is left as it is: provider list is the state,
# the key files are only read when a create is actually needed. The list is
# captured whole, so awk reads it from a here-string and no pipe can close
# early under pipefail. Only the first column is compared, so a provider named
# deepseek-legacy does not count as deepseek.
provider_exists() {
  local listed
  listed="$(openshell provider list 2>/dev/null || true)"
  awk -v name="$1" '$1 == name { found = 1 } END { exit !found }' <<<"$listed"
}

# profile_exists ID is the same question against profile list. profile import
# is not idempotent — it fails with "custom provider profile '<id>' already
# exists" — so a profile the gateway already lists is skipped rather than
# deleted and imported again.
profile_exists() {
  local listed
  # --global for the same reason import passes it: these are platform-scoped
  # profiles, which a workspace-scoped list leaves out.
  listed="$(openshell profile list --global 2>/dev/null || true)"
  awk -v name="$1" '$1 == name { found = 1 } END { exit !found }' <<<"$listed"
}

# profile_id FILE prints the id: a profile yaml declares, the name import keys
# on and the name the gateway reports a clash under. A line starting with
# "id:" only, so a nested "- id:" inside credentials is not mistaken for it.
profile_id() {
  awk '/^id:/ { sub(/^id:[[:space:]]*/, ""); sub(/[[:space:]#].*$/, ""); print; exit }' "$1"
}

# key_value FILE KEY prints the value of KEY in a KEY=value env file. The file
# is read as data: a line that is not an assignment to KEY is ignored, and
# surrounding quotes are stripped. Nothing in the file is executed.
key_value() {
  local file=$1 key=$2 line value
  line="$(grep -E "^(export )?$key=" "$file" | tail -n 1 || true)"
  value="${line#export }"
  value="${value#"$key="}"
  case "$value" in
    \"*\") value="${value#\"}"; value="${value%\"}" ;;
    \'*\') value="${value#\'}"; value="${value%\'}" ;;
  esac
  printf '%s' "$value"
}

# masked SECRET copies stdin to stdout with every occurrence of SECRET replaced
# by ***. It matches the secret itself, not a pattern, so a short key is hidden
# as surely as a long one.
masked() {
  local secret=$1 line
  while IFS= read -r line || [[ -n "$line" ]]; do
    printf '%s\n' "${line//"$secret"/***}"
  done
}

import_profiles() {
  local file id
  local profiles=()
  shopt -s nullglob
  profiles=("$PROFILES"/*.yaml)
  shopt -u nullglob
  if [[ ${#profiles[@]} -eq 0 ]]; then
    echo "openshell-providers: no profiles under ${PROFILES#"$ROOT"/}, nothing to import"
    return 0
  fi
  for file in "${profiles[@]}"; do
    id="$(profile_id "$file")"
    # A profile with no id: cannot be one the gateway already lists, so it
    # goes to lint and import as it always did and lint names what is wrong.
    if [[ -n "$id" ]] && profile_exists "$id"; then
      echo "openshell-providers: profile $id exists"
      continue
    fi
    openshell profile lint -f "$file"
    openshell profile import -f "$file" --global
  done
}

# create_provider NAME TYPE FILE KEY CREDENTIAL
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
    secret="$(key_value "$file" "$key")"
    if [[ -z "$secret" ]]; then
      echo "openshell-providers: $file does not set $key" >&2
      exit 1
    fi
    export "$credential=$secret"
    openshell provider create --name "$name" --type "$type" --credential "$credential" | masked "$secret"
  )
}

import_profiles
create_provider deepseek deepseek-claude "$CONFIG_DIR/deepseek.env" DEEPSEEK_API_KEY ANTHROPIC_AUTH_TOKEN
create_provider forgejo-polecat forgejo-polecat "$CONFIG_DIR/forgejo-polecat.env" FORGEJO_TOKEN FORGEJO_TOKEN
