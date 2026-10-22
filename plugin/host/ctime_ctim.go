//go:build unix && !(darwin || freebsd || netbsd)

package host

import (
	"io/fs"
	"syscall"
)

// changeTime returns fi's inode change time (ctime) in nanoseconds.
func changeTime(fi fs.FileInfo) (int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ctim.Nano(), true
}
