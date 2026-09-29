package doctor

// runBdSQLCSV runs `bd sql --csv <query>` in dir and returns the parsed CSV
// records from stdout (header row included). bd's stderr diagnostics stay out
// of the CSV (gt-m7t); a failure carries them (beads.Beads.SQLCSV).
func runBdSQLCSV(ctx *CheckContext, dir, query string) ([][]string, error) {
	return ctx.bd(dir, nil).SQLCSV(query)
}
