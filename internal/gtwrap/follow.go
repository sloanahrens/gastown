package gtwrap

import (
	"strconv"
	"strings"
)

// attentionValueFlags take the next argument as their value; a -f there is not a flag.
var attentionValueFlags = map[string]bool{"--since": true, "--interval": true}

// isFollowCall reports whether argv is a `gt attention` call that streams until killed.
// argv starts with the tool name, as rung2Allows expects; the last -f or --follow wins.
func isFollowCall(argv []string) bool {
	if len(argv) < 2 || argv[0] != "gt" || argv[1] != "attention" {
		return false
	}
	follow := false
	for i := 2; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			return follow
		case a == "-f" || a == "--follow":
			follow = true
		case strings.HasPrefix(a, "-f=") || strings.HasPrefix(a, "--follow="):
			// pflag refuses an unparseable value, so nothing streams.
			on, err := strconv.ParseBool(a[strings.IndexByte(a, '=')+1:])
			if err != nil {
				return false
			}
			follow = on
		case attentionValueFlags[a]:
			i++
		}
	}
	return follow
}
