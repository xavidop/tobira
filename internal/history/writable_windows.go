//go:build windows

package history

import (
	"fmt"
	"os"
)

// Writable reports whether tobira could record to the history file at
// path, without writing anything: it opens the file for writing (no
// truncation, no create) and closes it again.
func Writable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("the file is not writable: %w", err)
	}
	return f.Close()
}
