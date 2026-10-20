//go:build !darwin && !linux

package cmd

// processStart has no reader on this platform, so a reservation records no
// start time and falls back to polecatAdmissionReservationMaxAge.
func processStart(pid int) string { return "" }
