#!/usr/bin/env bash
# Imports the OpenShell profiles a polecat sandbox needs and creates the two
# providers they name, from the key files under ~/.config/gt (claude-q3f.9).
# Run it through `make openshell-providers` for the CLI's default gateway, or
# `scripts/openshell-providers.sh -g NAME` for a registered one such as k8s.
#
# A profile or provider the gateway already lists is left alone, so a rerun of
# the target changes nothing instead of failing on the first thing it meets.
#
# The forgejo-polecat profile binds the Forgejo endpoint of the gateway it is
# imported to. `gt sandbox endpoint` reads that endpoint from the town's sandbox
# profile and its policy file, and the script keeps no copy of the values. A
# gateway that already lists the profile on another host has it updated.
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

# The rendered forgejo-polecat profile lives in this run's temp dir only.
RENDER_DIR="$(mktemp -d)"
trap 'rm -rf "$RENDER_DIR"' EXIT

# GATEWAY empty means the gateway the CLI has as its default, which is what the
# script did before -g existed. The flag is global to openshell, so it has to
# come before the subcommand: every call goes through oshell.
GATEWAY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -g | --gateway)
      if [[ $# -lt 2 || -z "$2" ]]; then
        echo "openshell-providers: $1 needs a gateway name, e.g. -g k8s" >&2
        exit 2
      fi
      GATEWAY=$2
      shift 2
      ;;
    --gateway=*)
      GATEWAY=${1#--gateway=}
      if [[ -z "$GATEWAY" ]]; then
        echo "openshell-providers: --gateway needs a gateway name, e.g. --gateway=k8s" >&2
        exit 2
      fi
      shift
      ;;
    -h | --help)
      echo "usage: openshell-providers.sh [-g NAME]"
      exit 0
      ;;
    *)
      echo "openshell-providers: unknown argument: $1 (usage: openshell-providers.sh [-g NAME])" >&2
      exit 2
      ;;
  esac
done

if ! command -v openshell >/dev/null 2>&1; then
  echo "openshell-providers: openshell is not on PATH (brew install openshell)" >&2
  exit 1
fi

oshell() {
  if [[ -n "$GATEWAY" ]]; then
    openshell -g "$GATEWAY" "$@"
  else
    openshell "$@"
  fi
}

# A provider that already exists is left as it is: provider list is the state,
# the key files are only read when a create is actually needed. The list is
# captured whole, so awk reads it from a here-string and no pipe can close
# early under pipefail. Only the first column is compared, so a provider named
# deepseek-legacy does not count as deepseek.
provider_exists() {
  local listed
  listed="$(oshell provider list 2>/dev/null || true)"
  awk -v name="$1" '$1 == name { found = 1 } END { exit !found }' <<<"$listed"
}

# profile_exists ID is the same question against profile list. profile import
# is not idempotent — it fails with "custom provider profile '<id>' already
# exists" — so a profile the gateway already lists is skipped rather than
# deleted and imported again. Unlike provider_exists, a failed list stops the
# run: reading it as "absent" would send a profile the gateway already has to
# import, which then fails with a clash that hides the real cause.
profile_exists() {
  local listed
  # --global for the same reason import passes it: these are platform-scoped
  # profiles, which a workspace-scoped list leaves out.
  if ! listed="$(oshell profile list --global 2>&1)"; then
    echo "openshell-providers: openshell profile list --global failed, so whether profile $1 exists is unknown:" >&2
    printf '%s\n' "$listed" >&2
    exit 1
  fi
  awk -v name="$1" '$1 == name { found = 1 } END { exit !found }' <<<"$listed"
}

# gt_endpoint ARGS... runs `gt sandbox endpoint` for the gateway this run
# targets. The default gateway is named openshell, the name a sandbox profile
# without a gateway runs on. gt reads the endpoint from the town's sandbox
# profile and its policy; a refusal (no profile names the gateway, or the policy
# has no Forgejo rule) is gt's to explain, and its message reaches the terminal.
gt_endpoint() {
  gt sandbox endpoint --gateway "${GATEWAY:-openshell}" --root "$ROOT" "$@"
}

# read_endpoint reads the Forgejo endpoint this gateway's forgejo-polecat profile
# binds: the host the sandboxes reach Forgejo at, its port and its allowed_ips.
# A refusal stops the run before any profile or provider is touched.
read_endpoint() {
  local out key value
  if ! out="$(gt_endpoint)"; then
    echo "openshell-providers: gt sandbox endpoint --gateway ${GATEWAY:-openshell} failed, so the forgejo-polecat profile cannot be rendered" >&2
    exit 1
  fi
  ENDPOINT_HOST="" ENDPOINT_PORT="" ENDPOINT_IPS=""
  while IFS='=' read -r key value; do
    case "$key" in
      host) ENDPOINT_HOST=$value ;;
      port) ENDPOINT_PORT=$value ;;
      allowed_ips) ENDPOINT_IPS=$value ;;
    esac
  done <<<"$out"
  if [[ -z "$ENDPOINT_HOST" || -z "$ENDPOINT_PORT" ]]; then
    echo "openshell-providers: gt sandbox endpoint --gateway ${GATEWAY:-openshell} printed no host and port:" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
}

# render_forgejo_profile FILE OUT writes the forgejo-polecat profile FILE with the
# endpoint gt reads in place of the Docker host alias it names. gt renders it, so
# the Docker endpoint comes out byte for byte the same as FILE.
render_forgejo_profile() {
  if ! gt_endpoint --render "$1" >"$2"; then
    echo "openshell-providers: gt sandbox endpoint --render $1 failed, so the forgejo-polecat profile cannot be rendered" >&2
    exit 1
  fi
}

# profile_binds ID reports whether the gateway's profile ID binds the endpoint
# read above. profile export prints the profile the gateway holds, and it binds
# the endpoint when it names the host and each allowed_ips entry. A failed export
# stops the run, as a failed list does: guessing would either leave a wrong-host
# profile in place or update one that is already right.
profile_binds() {
  local exported ip
  local ips=()
  if ! exported="$(oshell profile export --global "$1" 2>&1)"; then
    echo "openshell-providers: openshell profile export --global $1 failed, so whether it binds $ENDPOINT_HOST is unknown:" >&2
    printf '%s\n' "$exported" >&2
    exit 1
  fi
  grep -qF -- "$ENDPOINT_HOST" <<<"$exported" || return 1
  if [[ -n "$ENDPOINT_IPS" ]]; then
    IFS=, read -ra ips <<<"$ENDPOINT_IPS"
    for ip in "${ips[@]}"; do
      grep -qF -- "$ip" <<<"$exported" || return 1
    done
  fi
}

# profile_id FILE prints the id: a profile yaml declares, the name import keys
# on and the name the gateway reports a clash under. A line starting with
# "id:" only, so a nested "- id:" inside credentials is not mistaken for it.
# A quoted id ("x" or 'x') prints without its quotes; an unquoted one ends at
# the first space or comment.
profile_id() {
  awk '/^id:/ {
    sub(/^id:[[:space:]]*/, "")
    if (match($0, /^"[^"]*"/) || match($0, /^\047[^\047]*\047/)) {
      print substr($0, 2, RLENGTH - 2)
    } else {
      sub(/[[:space:]#].*$/, "")
      print
    }
    exit
  }' "$1"
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
    # The file names the Docker host alias; the gateway it goes to may reach
    # Forgejo elsewhere, so the forgejo-polecat profile goes in as rendered.
    if [[ "$id" == forgejo-polecat ]]; then
      render_forgejo_profile "$file" "$RENDER_DIR/$id.yaml"
      file="$RENDER_DIR/$id.yaml"
    fi
    # A profile with no id: cannot be one the gateway already lists, so it
    # goes to lint and import as it always did and lint names what is wrong.
    if [[ -n "$id" ]] && profile_exists "$id"; then
      if [[ "$id" == forgejo-polecat ]] && ! profile_binds "$id"; then
        echo "openshell-providers: profile $id binds another host, updating it to $ENDPOINT_HOST"
        oshell profile lint -f "$file"
        oshell profile update --global -f "$file" "$id"
        continue
      fi
      echo "openshell-providers: profile $id exists"
      continue
    fi
    oshell profile lint -f "$file"
    oshell profile import -f "$file" --global
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
    oshell provider create --name "$name" --type "$type" --credential "$credential" | masked "$secret"
  )
}

read_endpoint
import_profiles
create_provider deepseek deepseek-claude "$CONFIG_DIR/deepseek.env" DEEPSEEK_API_KEY ANTHROPIC_AUTH_TOKEN
create_provider forgejo-polecat forgejo-polecat "$CONFIG_DIR/forgejo-polecat.env" FORGEJO_TOKEN FORGEJO_TOKEN
