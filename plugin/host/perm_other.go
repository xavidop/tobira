//go:build !unix

package host

import (
	"io/fs"
	"os"
)

// unsafePerm checks nothing where Unix permission bits do not apply.
func unsafePerm(string, fs.FileInfo) string { return "" }

// UnsafeDir checks only that dir is a directory where Unix permission
// bits do not apply.
func UnsafeDir(dir string) string {
	st, err := os.Stat(dir)
	if err != nil {
		return "cannot be checked: " + errText(err)
	}
	if !st.IsDir() {
		return "not a directory"
	}
	return ""
}
