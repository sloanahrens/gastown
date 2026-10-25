// Command trackerplist prints the launchd property list for one town tracker
// job. Installation writes the output to a file and loads it with launchctl:
//
//	trackerplist tracker-db > ~/Library/LaunchAgents/com.gastown.tracker-db.plist
//	trackerplist trackerd   > ~/Library/LaunchAgents/com.gastown.trackerd.plist
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "trackerplist:", err)
		os.Exit(1)
	}
}

// run prints the plist of the job named in args to stdout. getenv supplies
// HOME, which is the default -home.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("trackerplist", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "town home directory (default $HOME)")
	pgBin := fs.String("pg-bin", "/opt/homebrew/opt/postgresql@17/bin", "directory holding the postgres binary")
	port := fs.Int("port", 5433, "TCP port postgres listens on, loopback only")
	listen := fs.String("listen", "127.0.0.1:7433", "loopback address trackerd serves on")
	labelPrefix := fs.String("label-prefix", "com.gastown", "launchd label prefix")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: trackerplist [flags] tracker-db|trackerd")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *home == "" {
		*home = getenv("HOME")
	}
	cfg := Config{Home: *home, PGBin: *pgBin, Port: *port, Listen: *listen, LabelPrefix: *labelPrefix}
	if fs.NArg() != 1 {
		return errors.New("name one job to print: tracker-db or trackerd")
	}
	var (
		text string
		err  error
	)
	switch fs.Arg(0) {
	case "tracker-db":
		text, err = DatabasePlist(cfg)
	case "trackerd":
		text, err = TrackerdPlist(cfg)
	default:
		return fmt.Errorf("unknown job %q: name tracker-db or trackerd", fs.Arg(0))
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, text)
	return err
}
