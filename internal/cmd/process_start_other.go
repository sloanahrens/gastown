//go:build !linux

package cmd

// processStart has no reader on this platform, so a reservation records no
// start time and falls back to polecatAdmissionReservationMaxAge. Darwin is
// build-only since the gt-wrap cutover (gt-qq5kv4): nothing runs this there, so
// the empty result is not a supported runtime value.
func processStart(pid int) string { return "" }
