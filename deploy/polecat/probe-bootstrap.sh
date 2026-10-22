#!/usr/bin/env bash
# Phase 0 bootstrap, run INSIDE the sandbox with `openshell sandbox exec`.
# Prepares the clone and the bd/git plumbing that a polecat worktree gets for
# free on the host. Every step prints PASS/FAIL so the failures list is a grep.
set -u
REPO_URL=${REPO_URL:-http://host.docker.internal:3000/sloan/gastown.git}
BRANCH=${BRANCH:?polecat branch, e.g. polecat/sandbox-probe/gt-0hkdo+phase0}
BASE=${BASE:-main}
WORK=/sandbox/gastown

step() { printf '\n== %s\n' "$1"; }
ok()   { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; }

step "git identity and Forgejo header (placeholder in FORGEJO_TOKEN, rewritten by the supervisor)"
git config --global user.name  "sandbox-probe" \
 && git config --global user.email "sandbox-probe@gt.local" \
 && git config --global http.extraHeader "Authorization: token ${FORGEJO_TOKEN:-missing}" \
 && git config --global init.defaultBranch main && ok gitconfig || fail gitconfig

step "ls-remote Forgejo"
git ls-remote "$REPO_URL" "refs/heads/$BASE" >/tmp/lsremote.out 2>&1 && { ok ls-remote; head -1 /tmp/lsremote.out; } || { fail ls-remote; tail -3 /tmp/lsremote.out; }

step "clone $BASE and branch $BRANCH"
if [ ! -d "$WORK/.git" ]; then
  git clone --branch "$BASE" --single-branch "$REPO_URL" "$WORK" >/tmp/clone.out 2>&1 && ok clone || { fail clone; tail -5 /tmp/clone.out; }
fi
cd "$WORK" 2>/dev/null && git checkout -B "$BRANCH" >/dev/null 2>&1 && ok branch || fail branch

step "beads metadata (Dolt on the Mac)"
mkdir -p .beads && cat > .beads/metadata.json <<EOF
{"backend":"dolt","dolt_mode":"server","dolt_server_host":"${BEADS_DOLT_SERVER_HOST:-host.docker.internal}","dolt_server_port":${BEADS_DOLT_SERVER_PORT:-3307},"dolt_database":"gt"}
EOF
ok metadata

step "bd reaches Dolt"
timeout 60 bd version >/tmp/bdv.out 2>&1 && ok bd-version || { fail bd-version; tail -3 /tmp/bdv.out; }
timeout 60 bd show "${PROOF_BEAD:-gt-0hkdo}" >/tmp/bdshow.out 2>&1 && { ok bd-show; head -3 /tmp/bdshow.out; } || { fail bd-show; tail -5 /tmp/bdshow.out; }

step "go toolchain and module download"
timeout 300 go build ./cmd/gt >/tmp/gobuild.out 2>&1 && ok go-build || { fail go-build; tail -5 /tmp/gobuild.out; }

step "gt inside the fence"
timeout 60 gt version >/tmp/gtv.out 2>&1 && { ok gt-version; head -1 /tmp/gtv.out; } || { fail gt-version; tail -3 /tmp/gtv.out; }
timeout 60 gt prime --hook >/tmp/gtprime.out 2>&1 && { ok gt-prime; head -5 /tmp/gtprime.out; } || { fail gt-prime; tail -8 /tmp/gtprime.out; }

step "claude binary answers (no network)"
timeout 30 claude --version >/tmp/cv.out 2>&1 && ok claude-version || { fail claude-version; tail -2 /tmp/cv.out; }

step "negative: curl to Forgejo must be denied (not in policy binaries)"
timeout 15 curl -s -o /dev/null -w '%{http_code}' http://host.docker.internal:3000/ >/tmp/curl.out 2>&1 && { fail curl-denied "(got $(cat /tmp/curl.out))"; } || ok curl-denied
