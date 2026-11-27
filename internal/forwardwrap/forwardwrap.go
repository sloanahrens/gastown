// Package forwardwrap holds the shell wrapper a launchd job runs to keep a
// kubectl port-forward on one Running pod of a Service.
package forwardwrap

// Script is the wrapper, run as "/bin/sh -c Script sh client context namespace
// service bind port": $1 to $6 are those values, kept as positional arguments, so
// no value is ever read as script text, and $0 is the sh placeholder.
//
// It forwards the port to the first Running pod labeled
// app.kubernetes.io/name=<service> in the background, waits for the forward to
// end, then exits 1, so launchd's KeepAlive restarts it to resolve a new pod.
// When the get finds no pod, it names the selector on stderr and exits 1
// before starting a forward.
const Script = `pod=$("$1" --context="$2" --namespace="$3" get pods -l "app.kubernetes.io/name=$4" --field-selector=status.phase=Running -o name | head -n 1)
[ -n "$pod" ] || { echo "no Running pod for app.kubernetes.io/name=$4" >&2; exit 1; }
"$1" --context="$2" --namespace="$3" port-forward --address="$5" "$pod" "$6":"$6" &
p=$!
wait "$p"
exit 1`
