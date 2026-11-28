package townflight

import (
	"fmt"
	"os"
)

// entryLabel returns the label in the entry file at path. A file that cannot
// be read, or is empty, gives text naming path, so a drain can say which entry.
func entryLabel(path string) string { return entryLabelWith(os.ReadFile, path) }

// entryLabelWith is entryLabel with the read a test can fail.
func entryLabelWith(read func(string) ([]byte, error), path string) string {
	label, err := read(path)
	if err != nil || len(label) == 0 {
		return fmt.Sprintf("the command label in %s could not be read", path)
	}
	return string(label)
}
