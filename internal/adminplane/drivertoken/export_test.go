package drivertoken

// Records returns a copy of the records the store holds, so a test can read
// what was stored without holding the store's own slice.
func (m *Memory) Records() []Record {
	out := make([]Record, len(m.records))
	copy(out, m.records)
	return out
}
