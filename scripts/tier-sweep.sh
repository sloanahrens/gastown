#!/usr/bin/env bash
# tier-sweep runs the tiers `make gate` does not (gt-6ox58), one package at a
# time so no single run outlives its timeout, and prints one summary line per
# tier. It exits non-zero when any tier is red.
#
#   shell        $SHELL_TESTS (which make test-slow runs after the gate) and
#                the *_test.sh scripts it does not call, GT_TEST_DOCKER=0
#   integration  each package with a //go:build integration file, with
#                -tags integration, then the docker.txt packages untagged, as
#                make test-integration does; all under `gt slot run`. A nested
#                module's packages run in that module's directory, and the
#                root list leaves them out: the root toolchain cannot build
#                them (gt-558i2)
#   race         internal/cmd's command-tree race guard under -race in the
#                integration build, under `gt slot run`: the integration
#                TestMain must presort the cobra tree like the unit one does
#                (gt-jz03n.6), and only -race shows a regression
#
# Usage: scripts/tier-sweep.sh [shell] [integration] [race]   (default: all)
#
# TIER_SWEEP_SKIP is the known-red list: whitespace-separated entries of
#   <tier>:<pkg>             skip the package in that tier, e.g.
#                            integration:./internal/cmd, and a nested module's
#                            package as integration:./tracker/store
#   <tier>:<pkg>:<regexp>    run it with go test -skip <regexp>
# It defaults to empty: every tier is green (gt-6ox58), and the container
# trigger's entries join it (gt-e69b2).
# TIER_SWEEP_TRIGGER=0 turns the container trigger off, so every docker.txt
# package runs as it did before the trigger (gt-e69b2).
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
#
# A tier that counted nothing is RED: an empty script or package list means the
# tier's list broke, and GREEN reports a tier as passing when nothing ran.
# Skips alone are not RED — TIER_SWEEP_SKIP is deliberate. The line keeps the
# shape the daemon and the landing gate parse, so an empty tier is a RED
# summary with no names (gt-tqxdd).
summary() {
	local total=$(( $2 + $3 + $4 ))
	local verdict=GREEN note=""
	if [ "$3" -gt 0 ]; then
		verdict=RED; red=1; note="${5:+ failed: $5}"
	elif [ "$total" -eq 0 ]; then
		verdict=RED; red=1
	fi
	echo "tier-sweep: $1 $verdict passed=$2 failed=$3 skipped=$4$note (logs $TIER_SWEEP_LOGDIR) in $(fmt_duration "$6")"
}

# nested_modules prints this repo's nested module directories: every go.mod
# below the root, as the Makefile's NESTED_MODULES finds them. The pathspec
# names the file exactly: '*go.mod' also matches a file such as extra/notgo.mod,
# whose directory would then wrongly leave the root list.
nested_modules() {
	git ls-files '*/go.mod' | grep -v -e '^testdata/' -e '/testdata/' -e '^vendor/' -e '/vendor/' | awk -v OFS=/ -F/ 'NF > 1 { NF--; print }' | LC_ALL=C sort -u
}

# tagged_dirs prints ./path for each directory of THIS module (the cwd's) that
# holds a //go:build integration test file, ./ for one at the module root. The
# awk takes dirname's place because xargs runs it on an empty list too, and
# dirname with no operand prints an error a module with no tagged package would
# put in every sweep's log.
tagged_dirs() {
	git ls-files '*_test.go' | grep -v /testdata/ | xargs grep -lE '^//go:build.*(^|[^!])integration' | awk -v OFS=/ -F/ 'NF > 1 { NF--; print "./" $0; next } { print "./" }' | LC_ALL=C sort -u
}

# inside_module DIR MODULE... is true when DIR is one of the modules or below
# it. Both sides are ./repo-relative, as tagged_dirs and the module list print
# them.
inside_module() {
	local dir=$1 m
	shift
	for m in ${1+"$@"}; do
		case "$dir" in "$m"|"$m"/*) return 0 ;; esac
	done
	return 1
}

# go_tier TIER DIR PREFIX_CMD... :: GO_TEST_ARGS... :: PKGS... (the prefix may
# hold a "--", so "::" separates the lists). go test runs in DIR, the module
# PKGS belong to; PKGS are DIR-relative and a package outside the root reports
# as ./DIR/PKG, so a failure in tracker/store/pgtest reads
# failed: ./tracker/store/pgtest — the name the skip list and the log use too.
go_tier() {
	local tier=$1 dir=$2; shift 2
	local prefix=() args=() pkgs=()
	while [ "$1" != :: ]; do prefix+=("$1"); shift; done; shift
	while [ "$1" != :: ]; do args+=("$1"); shift; done; shift
	pkgs=("$@")
	local pass=0 fail=0 skipped=0 failed="" pkg name skip log extra
	for pkg in ${pkgs[@]+"${pkgs[@]}"}; do
		name=$pkg
		if [ "$dir" != . ]; then
			name="./$dir"
			[ "$pkg" = "./" ] || name="$name/${pkg#./}"
		fi
		skip=$(skip_for "$tier" "$name")
		if [ "$skip" = pkg ]; then skipped=$((skipped + 1)); echo "tier-sweep: $tier $name SKIPPED (TIER_SWEEP_SKIP)" >&2; continue; fi
		extra=()
		[ -n "$skip" ] && extra=(-skip "$skip")
		log="$TIER_SWEEP_LOGDIR/$tier-$(echo "${name#./}" | tr / _).log"
		echo "tier-sweep: $tier $name ..." >&2
		if ( cd "$dir" && ${prefix[@]+"${prefix[@]}"} go test -count=1 -timeout "$TIER_SWEEP_TIMEOUT" ${args[@]+"${args[@]}"} ${extra[@]+"${extra[@]}"} "$pkg" ) >"$log" 2>&1; then
			pass=$((pass + 1))
		else
			fail=$((fail + 1)); failed="${failed:+$failed }$name"
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
			if GT_TEST_DOCKER=0 bash "$t" >"$log" 2>&1; then p=$((p + 1)); else f=$((f + 1)); names="${names:+$names }$t"; tail -5 "$log" | sed 's/^/    /' >&2; fi
		done
		summary shell "$p" "$f" 0 "$names" "$((SECONDS - tier_start))"
		;;
	integration)
		# TIER_SWEEP_TESTED is the commit this run tests: the record stamps it,
		# and only while HEAD still is it (gt-e69b2).
		export TIER_SWEEP_TESTED="$(git rev-parse HEAD 2>/dev/null || true)"
		# The container trigger: the Go command decides which docker.txt packages
		# are due. It reads TIER_SWEEP_TRIGGER, names its own failures on stderr
		# and exits non-zero, which leaves every container package to run (gt-e69b2).
		export TIER_SWEEP_SKIP="$TIER_SWEEP_SKIP $(go run ./internal/tiertrigger/cmd/tiertrigger skips integration-docker)"
		# A module's list leaves out every directory inside one of its nested
		# modules: git ls-files sees those files, but the toolchain reports
		# "main module ... does not contain package" for them. Each nested
		# module runs its own tagged packages in its own directory instead.
		modules=($(nested_modules))
		mods=()
		for m in ${modules[@]+"${modules[@]}"}; do mods+=("./$m"); done
		tagged=()
		while read -r pkg; do
			[ -n "$pkg" ] || continue
			inside_module "$pkg" ${mods[@]+"${mods[@]}"} || tagged+=("$pkg")
		done < <(tagged_dirs)
		docker=($(sed -e 's/#.*//' internal/testpolicy/docker.txt | awk 'NF{print "./" $1}'))
		read -r p1 f1 s1 n1 < <(go_tier integration . gt slot run -- env GT_TEST_DOCKER=1 :: -tags integration :: ${tagged[@]+"${tagged[@]}"})
		read -r p2 f2 s2 n2 < <(go_tier integration-docker . gt slot run -- env GT_TEST_DOCKER=1 :: :: ${docker[@]+"${docker[@]}"})
		# Only a passing untagged run earns the record; a red one leaves the
		# state alone, so the next sweep runs those packages again (gt-e69b2).
		[ "$f2" -eq 0 ] && go run ./internal/tiertrigger/cmd/tiertrigger record integration-docker
		p=$((p1 + p2)) f=$((f1 + f2)) s=$((s1 + s2))
		# The modules' names come before the untagged ones: "untagged:"
		# labels the group that follows it, so a nested name appended later
		# would read as one of them. Each append stays guarded: appending an
		# empty nm unconditionally empties names, dropping the root run's.
		names="$n1"
		for m in ${modules[@]+"${modules[@]}"}; do
			deeper=()
			for o in ${mods[@]+"${mods[@]}"}; do case "$o" in "./$m/"*) deeper+=("$o") ;; esac; done
			pkgs=()
			while read -r mp; do
				[ -n "$mp" ] || continue
				repo="./$m"
				[ "$mp" = "./" ] || repo="$repo/${mp#./}"
				inside_module "$repo" ${deeper[@]+"${deeper[@]}"} || pkgs+=("$mp")
			done < <(cd "$m" && tagged_dirs)
			[ ${#pkgs[@]} -eq 0 ] && continue
			read -r pm fm sm nm < <(go_tier integration "$m" gt slot run -- env GT_TEST_DOCKER=1 :: -tags integration :: "${pkgs[@]}")
			p=$((p + pm)) f=$((f + fm)) s=$((s + sm))
			[ -n "$nm" ] && names="${names:+$names }$nm"
		done
		[ -n "$n2" ] && names="${names:+$names }untagged:$n2"
		summary integration "$p" "$f" "$s" "$names" "$((SECONDS - tier_start))"
		;;
	race)
		read -r p f s names < <(go_tier race . gt slot run -- env GT_TEST_DOCKER=1 :: -race -tags integration -run '^TestCommandTreeWalkIsReadOnlyUnderParallelTests$' :: ./internal/cmd)
		summary race "$p" "$f" "$s" "$names" "$((SECONDS - tier_start))"
		;;
	*)
		echo "tier-sweep: unknown tier $tier (want shell, integration or race)" >&2
		exit 2
		;;
	esac
done
exit $red
