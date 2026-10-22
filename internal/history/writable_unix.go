//go:build !windows

package history

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Writable reports whether tobira could record to the history file at
// path, without writing anything: the file must be writable, and its
// directory writable and searchable, since SQLite writes its journal next
// to the file. It checks with access(2), so nothing (mode, contents, mtime)
// changes.
func Writable(path string) error {
	if err := unix.Access(path, unix.W_OK); err != nil {
		return fmt.Errorf("the file is not writable: %w", err)
	}
	if err := unix.Access(filepath.Dir(path), unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("its directory is not writable: %w", err)
	}
	return nil
}
