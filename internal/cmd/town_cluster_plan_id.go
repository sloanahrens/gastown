package cmd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// planFile is one file of a cluster plan: the name it appears under and its
// bytes, in the order the plan applies them.
type planFile struct {
	Name string
	Data []byte
}

// planIDShortHexLen is how many hex digits the short form of a plan id keeps:
// enough to say a plan's name in conversation, and the full id's prefix.
const planIDShortHexLen = 12

// planID returns the id of the plan built from files in their apply order: the
// hex SHA-256, and its first planIDShortHexLen hex digits. Each file contributes
// its name, a zero byte, its length as eight big-endian bytes, then its bytes, so
// a rename, a reorder or a changed byte changes the id. The fixed-width length
// keeps the boundary between two files unambiguous whatever their names and
// bytes contain, so the same sequence recomputes the same id on any platform.
func planID(files []planFile) (full, short string) {
	h := sha256.New()
	var length [8]byte
	for _, f := range files {
		h.Write([]byte(f.Name))
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(length[:], uint64(len(f.Data)))
		h.Write(length[:])
		h.Write(f.Data)
	}
	full = hex.EncodeToString(h.Sum(nil))
	return full, full[:planIDShortHexLen]
}
