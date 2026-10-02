#!/usr/bin/env bash
# flake-sweep repeats the unit tier under concurrent load and tallies which
# tests failed, so a flake the gate's single pass never sees arrives with a
# count and the log that first caught it (gt-22hdp.64).
#
# Usage: scripts/flake-sweep.sh [ITER] [CONC]   (default: 20 2)
#
# Each iteration starts CONC copies at once of the unit tier exactly as `make
# gate` runs it (Makefile gate-test), plus -count=1 so the test cache cannot
# hide a flake and -json so the tally can read it. Every copy runs under
# `nice -n 10` and logs to its own file under FLAKE_SWEEP_LOGDIR (default: a
# new temp dir). The concurrency is the load gt-22hdp.19 measures
# parallelism with.
#
# The sweep stays off `gt slot run`: it takes no container slot, runs no
# GT_TEST_DOCKER=1 copy, and so may run beside a landing gate (gt-22hdp.63).
#
# A run's exit code is what makes it failed; the logs are read only to name the
# tests, never to decide the verdict. Exit 1 when any run failed, 0 when none,
# 2 on a usage or setup error. ITER CONC of 20 2 is hours: the unit tier is
# uncached, and two copies share the host.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

if [ $# -gt 2 ]; then
	echo "flake-sweep: usage: flake-sweep.sh [ITER] [CONC] (default 20 2)" >&2
	exit 2
fi
iter=${1:-20}
conc=${2:-2}
case $iter in ''|*[!0-9]*) echo "flake-sweep: ITER must be a positive integer, got '$iter'" >&2; exit 2 ;; esac
case $conc in ''|*[!0-9]*) echo "flake-sweep: CONC must be a positive integer, got '$conc'" >&2; exit 2 ;; esac
if [ "$iter" -lt 1 ] || [ "$conc" -lt 1 ]; then
	echo "flake-sweep: ITER and CONC must be at least 1, got '$iter' '$conc'" >&2
	exit 2
fi
command -v go >/dev/null || { echo "flake-sweep: go is not on PATH" >&2; exit 2; }
command -v nice >/dev/null || { echo "flake-sweep: nice is not on PATH" >&2; exit 2; }

if [ -z "${FLAKE_SWEEP_LOGDIR:-}" ]; then
	FLAKE_SWEEP_LOGDIR=$(mktemp -d "${TMPDIR:-/tmp}/flake-sweep.XXXXXX") || { echo "flake-sweep: cannot create a log dir" >&2; exit 2; }
fi
mkdir -p "$FLAKE_SWEEP_LOGDIR" || { echo "flake-sweep: cannot write FLAKE_SWEEP_LOGDIR '$FLAKE_SWEEP_LOGDIR'" >&2; exit 2; }
tally=$(mktemp "${TMPDIR:-/tmp}/flake-sweep-tally.XXXXXX") || { echo "flake-sweep: cannot create the tally file" >&2; exit 2; }

cleanup() { rm -f "$tally"; kill $(jobs -p) 2>/dev/null; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM

# extract_failures LOG prints one "test<TAB>package<TAB>log" line per failing
# test and per package-level failure in the -json log. grep -a: a go test log
# can classify as binary, and a silent no-match would read as zero flakes.
extract_failures() {
	local line test pkg
	while IFS= read -r line; do
		pkg=$(sed -n 's/.*"Package":"\([^"]*\)".*/\1/p' <<<"$line")
		case $line in
		*'"Test":'*) test=$(sed -n 's/.*"Test":"\([^"]*\)".*/\1/p' <<<"$line") ;;
		*) test="$pkg (package)" ;;
		esac
		printf '%s\t%s\t%s\n' "$test" "$pkg" "$1"
	done < <(grep -a '"Action":"fail"' "$1")
}

# print_report prints one row per failing test: its package, the share of the M
# runs it failed in, and the log that first caught it, worst first.
print_report() {
	[ -s "$tally" ] || return 0
	awk -F'\t' -v total="$runs" '
		{
			key = $1 "\t" $2
			if (!(key in first)) { first[key] = $3; keys[++n] = key }
			count[key]++
		}
		END {
			for (i = 1; i <= n; i++)
				for (j = i + 1; j <= n; j++)
					if (count[keys[j]] > count[keys[i]] ||
					    (count[keys[j]] == count[keys[i]] && keys[j] < keys[i])) {
						swap = keys[i]; keys[i] = keys[j]; keys[j] = swap
					}
			printf "%-58s %-42s %-12s %s\n", "test", "package", "failures/" total, "first_log"
			for (i = 1; i <= n; i++) {
				split(keys[i], col, "\t")
				printf "%-58s %-42s %-12s %s\n", col[1], col[2], count[keys[i]] "/" total, first[keys[i]]
			}
		}
	' "$tally"
}

runs=0
failed_runs=0
echo "flake-sweep: $iter iterations x $conc concurrent copies, logs in $FLAKE_SWEEP_LOGDIR" >&2

for ((i = 1; i <= iter; i++)); do
	pids=()
	logs=()
	for ((j = 1; j <= conc; j++)); do
		log="$FLAKE_SWEEP_LOGDIR/iter$(printf '%02d' "$i")-run$(printf '%02d' "$j").json"
		logs+=("$log")
		# The unit tier is the timeout the hang detector needs (gt-ik4a1.1);
		# -json goes to stdout, so the log holds the whole run.
		GT_TEST_DOCKER=0 nice -n 10 go test -timeout 20m -count=1 -json ./... >"$log" 2>&1 &
		pids+=($!)
		runs=$((runs + 1))
	done
	iter_failed=0
	for k in "${!pids[@]}"; do
		rc=0
		wait "${pids[$k]}" || rc=$?
		[ "$rc" -eq 0 ] && continue
		failed_runs=$((failed_runs + 1))
		iter_failed=$((iter_failed + 1))
		out=$(extract_failures "${logs[$k]}")
		if [ -n "$out" ]; then
			printf '%s\n' "$out" >>"$tally"
		else
			printf 'go test (log names no failing test)\t-\t%s\n' "${logs[$k]}" >>"$tally"
		fi
	done
	echo "flake-sweep: iteration $i/$iter done, $iter_failed failing run(s)" >&2
done

echo "flake-sweep: iterations=$iter runs=$runs failed_runs=$failed_runs"
print_report
[ "$failed_runs" -gt 0 ] && exit 1
exit 0
