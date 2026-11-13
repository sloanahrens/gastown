// Command bd is the stand-in bd that TestExecutorRealProcess runs: "hold"
// waits to be killed, any other argument is the status to exit with after one
// line on each stream.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		// A timer keeps the runtime's deadlock detector quiet while the
		// executor has not killed the process yet.
		time.Sleep(10 * time.Minute)
		return
	}
	status := 0
	if len(os.Args) > 1 {
		n, err := strconv.Atoi(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bd stand-in: status %q is not a number\n", os.Args[1])
			os.Exit(2)
		}
		status = n
	}
	fmt.Fprintln(os.Stdout, "out")
	fmt.Fprintln(os.Stderr, "err")
	os.Exit(status)
}
