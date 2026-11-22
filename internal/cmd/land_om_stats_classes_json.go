package cmd

// The --classes report as --json prints it (2d/5, gt-9ofql6): the base report's
// own fields, unchanged, and the class-rate rows under one "classes" key.

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/land"
)

// landOmStatsClassesReport is what --classes --json prints: the base report's
// fields as a run without the flag prints them, plus one entry per ISO week.
type landOmStatsClassesReport struct {
	land.OmStats
	Classes []landOmStatsClassWeek `json:"classes"`
}

// writeLandOmStatsClassesJSON prints the report and the class rows as one
// document, so a reader takes both without scraping the text table.
func writeLandOmStatsClassesJSON(w io.Writer, report landOmStatsClassesReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}
