package gocacheprog

// gainedSince returns the reread function applyEviction takes: it reads the
// journals of dir again and returns the action and output ids of every record
// not in first, so an entry journaled since the first read is kept. first holds
// that read's records keyed by utcRecord; a use contributes its action and the
// output idx names, a put its action, its own output and that indexed output. A
// journal read failure is returned.
func gainedSince(dir, ownPath string, first map[Record]bool, idx Index) func() (gainedActions, gainedOutputs [][]byte, err error) {
	return func() (gainedActions, gainedOutputs [][]byte, err error) {
		records, _, _, err := readJournals(dir, ownPath)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range records {
			if first[utcRecord(r)] {
				continue
			}
			gainedActions = append(gainedActions, []byte(r.ActionID))
			if e, ok := idx[r.ActionID]; ok {
				gainedOutputs = append(gainedOutputs, []byte(e.OutputID))
			}
			if r.Op == opPut {
				gainedOutputs = append(gainedOutputs, []byte(r.OutputID))
			}
		}
		return gainedActions, gainedOutputs, nil
	}
}

// utcRecord returns r with its time in UTC, so a record read twice compares equal.
func utcRecord(r Record) Record {
	r.Time = r.Time.UTC()
	return r
}
