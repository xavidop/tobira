//go:build unix

package host

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// unsafePerm says why a plugin binary others can replace must not run: the
// file is world-writable, or its directory is world-writable without the
// sticky bit and an owner that is root or this user (as /tmp has). It
// returns "" when the binary is fine.
func unsafePerm(path string, st fs.FileInfo) string {
	if st.Mode().Perm()&0o002 != 0 {
		return "world-writable"
	}
	dir := filepath.Dir(path)
	dst, err := os.Stat(dir)
	if err != nil {
		return "in a directory that cannot be checked"
	}
	if dst.Mode().Perm()&0o002 == 0 {
		return ""
	}
	if dst.Mode()&fs.ModeSticky != 0 {
		if sys, ok := dst.Sys().(*syscall.Stat_t); ok && (sys.Uid == 0 || int(sys.Uid) == os.Getuid()) {
			return ""
		}
	}
	return "in a world-writable directory"
}

// UnsafeDir says why dir must not hold plugins, or returns "" when it is
// fine: it cannot be checked, is not a directory, others (the world or
// its group) can write to it, or it belongs to a user other than this one
// or root. It is stricter than the launch check: a sticky bit does not
// help, since others could still add binaries of their own. `tobira
// driver install` refuses such a plugins directory.
func UnsafeDir(dir string) string {
	st, err := os.Stat(dir)
	if err != nil {
		return "cannot be checked: " + errText(err)
	}
	if !st.IsDir() {
		return "not a directory"
	}
	switch perm := st.Mode().Perm(); {
	case perm&0o002 != 0:
		return "world-writable"
	case perm&0o020 != 0:
		return "group-writable"
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 && int(sys.Uid) != os.Getuid() {
		return "owned by another user"
	}
	return ""
}
