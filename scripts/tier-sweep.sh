#!/usr/bin/env bash
# tier-sweep runs the tiers `make gate` does not (gt-6ox58), one package at a
# time so no single run outlives its timeout, and prints one summary line per
# tier. It exits non-zero when any tier is red.
#
#   shell        $SHELL_TESTS (which make test-slow runs after the gate) and
#                the *_test.sh scripts it does not call, GT_TEST_DOCKER=0
#   integration  each package with a //go:build integration file, with
#                -tags integration, then the docker.txt packages untagged, as
#                make test-integration does; all under `gt slot run`
#   race         internal/cmd's command-tree race guard under -race in the
#                integration build, under `gt slot run`: the integration
#                TestMain must presort the cobra tree like the unit one does
#                (gt-jz03n.6), and only -race shows a regression
#
# Usage: scripts/tier-sweep.sh [shell] [integration] [race]   (default: all)
#
# TIER_SWEEP_SKIP is the known-red list: whitespace-separated entries of
#   <tier>:<pkg>             skip the package in that tier, e.g.
#                            integration:./internal/cmd
#   <tier>:<pkg>:<regexp>    run it with go test -skip <regexp>
# It defaults to empty: every tier is green (gt-6ox58).
# TIER_SWEEP_TIMEOUT is the per-package go test timeout (default 10m).
# TIER_SWEEP_LOGDIR holds one log per run (default: a new temp dir).
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

: "${TIER_SWEEP_SKIP=}"
: "${TIER_SWEEP_TIMEOUT:=10m}"
: "${TIER_SWEEP_LOGDIR:=$(mktemp -d "${TMPDIR:-/tmp}/tier-sweep.XXXXXX")}"
mkdir -p "$TIER_SWEEP_LOGDIR"

tiers=("$@")
[ ${#tiers[@]} -eq 0 ] && tiers=(shell integration race)

# skip_for TIER PKG prints "pkg" when the package is skipped whole, the -skip
# regexp when only some tests are, and nothing otherwise.
skip_for() {
	local entry
	for entry in $TIER_SWEEP_SKIP; do
		case "$entry" in
		"$1:$2") echo pkg; return ;;
		"$1:$2:"*) echo "${entry#"$1:$2:"}"; return ;;
		esac
	done
}

red=0
# fmt_duration SECONDS renders a whole-second duration the way the daemon logs
# render theirs (time.Duration.String() after Round(time.Second)): 45s, 2m14s,
# 1h1m1s. The sweep's summary line and the daemon's line for the same tier then
# read alike in `gt tail`.
fmt_duration() {
	local s=$1
	if [ "$s" -ge 3600 ]; then
		printf '%dh%dm%ds' "$((s / 3600))" "$(((s % 3600) / 60))" "$((s % 60))"
	elif [ "$s" -ge 60 ]; then
		printf '%dm%ds' "$((s / 60))" "$((s % 60))"
	else
		printf '%ds' "$s"
	fi
}

# summary TIER PASSED FAILED SKIPPED FAILED_NAMES SECONDS. The elapsed time
# ends the line, so a slow or hung tier reads as such without opening its log.
summary() {
	local verdict=GREEN
	[ "$3" -gt 0 ] && { verdict=RED; red=1; }
	echo "tier-sweep: $1 $verdict passed=$2 failed=$3 skipped=$4${5:+ failed:$5} (logs $TIER_SWEEP_LOGDIR) in $(fmt_duration "$6")"
}

# go_tier TIER PREFIX_CMD... :: GO_TEST_ARGS... :: PKGS... (the prefix may hold
# a "--", so "::" separates the lists)
go_tier() {
	local tier=$1; shift
	local prefix=() args=() pkgs=()
	while [ "$1" != :: ]; do prefix+=("$1"); shift; done; shift
	while [ "$1" != :: ]; do args+=("$1"); shift; done; shift
	pkgs=("$@")
	local pass=0 fail=0 skipped=0 failed="" pkg skip log extra
	for pkg in "${pkgs[@]}"; do
		skip=$(skip_for "$tier" "$pkg")
		if [ "$skip" = pkg ]; then skipped=$((skipped + 1)); echo "tier-sweep: $tier $pkg SKIPPED (TIER_SWEEP_SKIP)" >&2; continue; fi
		extra=()
		[ -n "$skip" ] && extra=(-skip "$skip")
		log="$TIER_SWEEP_LOGDIR/$tier-$(echo "${pkg#./}" | tr / _).log"
		echo "tier-sweep: $tier $pkg ..." >&2
		if "${prefix[@]}" go test -count=1 -timeout "$TIER_SWEEP_TIMEOUT" ${args[@]+"${args[@]}"} ${extra[@]+"${extra[@]}"} "$pkg" >"$log" 2>&1; then
			pass=$((pass + 1))
		else
			fail=$((fail + 1)); failed="$failed $pkg"
			grep -a -E '^(--- FAIL|panic:|FAIL)' "$log" | head -5 | sed 's/^/    /' >&2
		fi
	done
	printf '%s %s %s %s\n' "$pass" "$fail" "$skipped" "$failed"
}

# Package paths hold no whitespace, so the lists below split on it.
# shellcheck disable=SC2207
for tier in "${tiers[@]}"; do
	tier_start=$SECONDS
	case "$tier" in
	shell)
		shell_tests=${SHELL_TESTS:-scripts/test-makefile.sh}
		scripts=("$shell_tests")
		while read -r t; do
			grep -qF "$t" "$shell_tests" || scripts+=("$t")
		done < <(git ls-files '*_test.sh' | grep -v -e '/testdata/' -e '^scripts/makefile-gate_test.sh$')
		p=0 f=0 names=""
		for t in "${scripts[@]}"; do
			log="$TIER_SWEEP_LOGDIR/shell-$(echo "$t" | tr / _).log"
			echo "tier-sweep: shell $t ..." >&2
			if GT_TEST_DOCKER=0 bash "$t" >"$log" 2>&1; then p=$((p + 1)); else f=$((f + 1)); names="$names $t"; tail -5 "$log" | sed 's/^/    /' >&2; fi
		done
		summary shell "$p" "$f" 0 "$names" "$((SECONDS - tier_start))"
		;;
	integration)
		tagged=($(git ls-files '*_test.go' | grep -v /testdata/ | xargs grep -lE '^//go:build.*(^|[^!])integration' | xargs -n1 dirname | sort -u | sed 's|^|./|'))
		docker=($(sed -e 's/#.*//' internal/testpolicy/docker.txt | awk 'NF{print "./" $1}'))
		read -r p1 f1 s1 n1 < <(go_tier integration gt slot run -- env GT_TEST_DOCKER=1 :: -tags integration :: "${tagged[@]}")
		read -r p2 f2 s2 n2 < <(go_tier integration-docker gt slot run -- env GT_TEST_DOCKER=1 :: :: "${docker[@]}")
		summary integration "$((p1 + p2))" "$((f1 + f2))" "$((s1 + s2))" "$n1${n2:+ untagged:$n2}" "$((SECONDS - tier_start))"
		;;
	race)
		read -r p f s names < <(go_tier race gt slot run -- env GT_TEST_DOCKER=1 :: -race -tags integration -run '^TestCommandTreeWalkIsReadOnlyUnderParallelTests$' :: ./internal/cmd)
		summary race "$p" "$f" "$s" "$names" "$((SECONDS - tier_start))"
		;;
	*)
		echo "tier-sweep: unknown tier $tier (want shell, integration or race)" >&2
		exit 2
		;;
	esac
done
exit $red
