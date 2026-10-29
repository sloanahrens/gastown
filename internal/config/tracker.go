package config

import (
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultTrackerPort is the port trackerd listens on when the town names
// none. It is the tracker's own constant: nothing else defines this port, so
// it must never carry Dolt's 3307.
const DefaultTrackerPort = 7433

// DefaultTrackerHost is the host a town without a tracker.host reaches: the
// local machine, where trackerd listens by default.
const DefaultTrackerHost = "127.0.0.1"

// ResolveTrackerEndpoint is the one resolver for the town's tracker endpoint.
// It reads durable config only, never the environment:
//
//  1. mayor/town.json "tracker" (written by gt install and gt config set
//     tracker.port/tracker.host)
//  2. the default endpoint when the town names no tracker: DefaultTrackerHost
//     on DefaultTrackerPort, which is where a default trackerd listens
//
// A town.json that fails to load is an error, never the default: a malformed
// port must not read as a working endpoint.
func ResolveTrackerEndpoint(townRoot string) (TrackerEndpoint, error) {
	defaultEndpoint := TrackerEndpoint{Port: DefaultTrackerPort}
	if townRoot == "" {
		return defaultEndpoint, nil
	}
	town, err := LoadTownConfig(filepath.Join(townRoot, "mayor", "town.json"))
	if errors.Is(err, ErrNotFound) {
		return defaultEndpoint, nil
	}
	if err != nil {
		return TrackerEndpoint{}, err
	}
	if town.Tracker == nil {
		return defaultEndpoint, nil
	}
	return TrackerEndpoint{Host: strings.TrimSpace(town.Tracker.Host), Port: town.Tracker.Port}, nil
}

// HostOrLocal is the host the endpoint reaches: the configured host, or the
// local machine when the town names none.
func (e TrackerEndpoint) HostOrLocal() string {
	if host := strings.TrimSpace(e.Host); host != "" {
		return host
	}
	return DefaultTrackerHost
}

// URL is the endpoint as the tracker client takes it. A host that is not a
// literal loopback IP passes through as written; the client validates it.
func (e TrackerEndpoint) URL() string {
	return "http://" + net.JoinHostPort(e.HostOrLocal(), strconv.Itoa(e.Port))
}
