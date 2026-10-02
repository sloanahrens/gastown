package cmd

import "time"

// noSleep stands in for time.Sleep so retry loops in tests never wait.
func noSleep(time.Duration) {}
