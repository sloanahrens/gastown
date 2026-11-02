package bdshim

import (
	"errors"
	"fmt"
)

// The second auxiliary handler: kv for a tracker rig (gt-p85pw.119). A tracker
// has no key-value store yet (internal/beads/trackerclient/admin.go: KVList
// answers an empty store and KVGet reports every key unset), so the shim
// answers the two reads from that answer and refuses every write by name: a
// write that cannot be stored is never reported as done.
//
// kv is one verb with three subcommands, so the handler reads which one the
// argv names before it does anything: list and get are answered, and set and
// any other subcommand are refused by name before their flags or operands are
// read as the answered commands'.

// The flag set each kv subcommand answers: none. The goldens record both
// commands without a flag, so a flag is refused rather than ignored.
var (
	kvListFlags = readFlagSpec{}
	kvGetFlags  = readFlagSpec{}
)

// auxKvStore is the part of a rig's store the kv handlers read. The handlers
// open the full backend.Store; the narrow interface lets a test drive each
// refusal without a tracker server.
type auxKvStore interface {
	KVList() (map[string]string, error)
	KVGet(key string) (string, error)
}

// kvHandler answers bd kv list and kv get for a tracker rig, and refuses every
// other kv subcommand by name.
func kvHandler(env Env, d Decision) int {
	switch command := kvCommand(env.Args); command {
	case "kv list":
		return kvListCommand(env, d)
	case "kv get":
		return kvGetCommand(env, d)
	default:
		return shimError(env, fmt.Errorf("%s is not implemented for tracker rig %s", command, d.Route.Prefix))
	}
}

// kvCommand names the kv command the argv asks for: the verb and the word
// after it, which is bd's subcommand. commandPath is not enough here: kv list
// takes no operand, so bd's table pins no row for it and commandPath stops at
// the verb. The subcommand is read from the operands instead, and a word bd
// does not document is still named as the caller's subcommand.
func kvCommand(args []string) string {
	ops := operands("", args)
	if len(ops) == 0 {
		return "kv"
	}
	if len(ops) == 1 {
		return ops[0]
	}
	return ops[0] + " " + ops[1]
}

// kvListCommand answers bd kv list for a tracker rig.
func kvListCommand(env Env, d Decision) int {
	fl, err := parseReadArgs("kv list", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, kvListFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("kv list takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return listKV(env, st, d.Route.Prefix)
}

// listKV prints bd's answer for an empty key-value store, the only answer the
// goldens record. A store that returned entries is refused rather than printed
// as empty: the shim does not report a store as empty when it is not, and bd's
// shape for a non-empty list is not recorded.
func listKV(env Env, st auxKvStore, prefix string) int {
	pairs, err := st.KVList()
	if err != nil {
		return shimError(env, fmt.Errorf("kv list: %w", err))
	}
	if len(pairs) > 0 {
		return shimError(env, fmt.Errorf("kv list with entries is not implemented for tracker rig %s", prefix))
	}
	if _, err := fmt.Fprint(env.Stdout, "No key-value pairs set\n"); err != nil {
		return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
	}
	return 0
}

// kvGetCommand answers bd kv get for a tracker rig.
func kvGetCommand(env Env, d Decision) int {
	fl, err := parseReadArgs("kv get", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, kvGetFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("kv get needs a key"))
	case 1:
	default:
		return shimError(env, fmt.Errorf("kv get takes one key; %q is not implemented for tracker rig %s", fl.operands[1], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return getKV(env, st, d.Route.Prefix, fl.operands[0])
}

// getKV answers a key the store does not hold as bd does: bd prints nothing on
// stdout for an unset key and exits 1, so the store's error is the whole
// answer and the shim prints its own message naming the key. The goldens record
// only that answer; a store that found a value is refused rather than printed
// in a shape bd did not record.
func getKV(env Env, st auxKvStore, prefix, key string) int {
	if _, err := st.KVGet(key); err != nil {
		return shimError(env, fmt.Errorf("kv get %s: %w", key, err))
	}
	return shimError(env, fmt.Errorf("kv get of a set key is not implemented for tracker rig %s", prefix))
}
