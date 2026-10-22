//go:build !unix

package host

import "io/fs"

// changeTime is unavailable here; the modification time still counts.
func changeTime(fs.FileInfo) (int64, bool) { return 0, false }
