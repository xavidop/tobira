package host

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Prefix is the file name prefix of a driver plugin binary.
const Prefix = "tobira-driver-"

// nameRE is the whole-name pattern of a plugin binary. It is never mutated.
var nameRE = regexp.MustCompile(`^tobira-driver-[a-z0-9][a-z0-9-]{0,62}$`)

// Plugin is a discovered driver plugin binary.
type Plugin struct {
	// Name is the driver name: the file name after "tobira-driver-"
	// (and before ".exe" on Windows). It is the registry key.
	Name string
	// Path is the binary's absolute path with symlinks resolved.
	Path string
	// Dir is the search directory the binary was found in.
	Dir string
	// stat is the binary as Discover saw it; the host refuses to start it
	// if the file has changed since (see sameFile) (nil for a Plugin built by hand, which
	// Register snapshots instead).
	stat os.FileInfo
}

// ValidName reports whether file (a base name) names a driver plugin and
// returns its driver name. A name must match
// ^tobira-driver-[a-z0-9][a-z0-9-]{0,62}$; on Windows a trailing ".exe" is
// ignored. `tobira driver install` uses the same check.
func ValidName(file string) (name string, ok bool) { return validName(file, runtime.GOOS) }

func validName(file, goos string) (string, bool) {
	if goos == "windows" && len(file) > 4 && strings.EqualFold(file[len(file)-4:], ".exe") {
		file = file[:len(file)-4]
	}
	if !nameRE.MatchString(file) {
		return "", false
	}
	return strings.TrimPrefix(file, Prefix), true
}

// SearchDirs is the discovery order: the plugins directories, then every
// entry of pathEnv (a PATH value), without duplicates or empty entries. A
// plugins directory therefore wins over PATH on a duplicate name.
func SearchDirs(pluginDirs []string, pathEnv string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range append(append([]string(nil), pluginDirs...), filepath.SplitList(pathEnv)...) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Discover lists the driver plugins in dirs, searched in order: the first
// binary found for a name wins. It is DiscoverWithLogger without a logger.
func Discover(dirs []string) []Plugin { return DiscoverWithLogger(dirs, nil) }

// DiscoverWithLogger is Discover, logging every skipped candidate (a
// tobira-driver-* entry that is badly named, not a regular executable file
// that only its owner can replace,
// or shadowed by an earlier directory) at debug level to log (nil discards).
// Relative directories, such as "." in PATH, are never searched: a plugin
// is code that runs, so it must not come from wherever the shell happens to
// be. Discover never starts a plugin.
func DiscoverWithLogger(dirs []string, log *slog.Logger) []Plugin {
	found, _ := DiscoverAll(dirs, log)
	return found
}

// DiscoverAll is DiscoverWithLogger that also returns the usable plugins
// it skipped because an earlier directory has one of the same name (a
// plugins directory shadowing PATH, say), in search order. `tobira driver
// list` warns about them.
func DiscoverAll(dirs []string, log *slog.Logger) (found, shadowed []Plugin) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	seen := map[string]string{} // name -> path
	for _, dir := range dirs {
		if dir == "" || !filepath.IsAbs(dir) {
			if dir != "" {
				log.Debug("plugin discovery: skipping a relative directory", "dir", dir)
			}
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			file := e.Name()
			if !strings.HasPrefix(strings.ToLower(file), Prefix) {
				continue // not a candidate: stay silent about the rest of PATH
			}
			full := filepath.Join(dir, file)
			name, ok := ValidName(file)
			if !ok {
				log.Debug("plugin discovery: skipping a badly named file", "path", full)
				continue
			}
			resolved, st, why := executable(full)
			if why != "" {
				log.Debug("plugin discovery: skipping", "path", full, "reason", why)
				continue
			}
			p := Plugin{Name: name, Path: resolved, Dir: dir, stat: st}
			if first, dup := seen[name]; dup {
				log.Debug("plugin discovery: skipping a shadowed plugin", "path", full, "winner", first)
				shadowed = append(shadowed, p)
				continue
			}
			seen[name] = resolved
			found = append(found, p)
		}
	}
	return found, shadowed
}

// executable resolves path's symlinks and checks that the target is a
// regular, executable file that others cannot replace (see unsafePerm). It
// returns the resolved absolute path and its FileInfo, or why it is not
// usable.
func executable(path string) (string, os.FileInfo, string) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, "cannot resolve: " + errText(err)
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return "", nil, "cannot resolve: " + errText(err)
	}
	st, why := checkFile(resolved)
	return resolved, st, why
}

// checkFile checks a resolved plugin path, at discovery and again at launch.
func checkFile(path string) (os.FileInfo, string) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, "cannot stat: " + errText(err)
	}
	if !st.Mode().IsRegular() {
		return nil, "not a regular file"
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		return nil, "not executable"
	}
	if why := unsafePerm(path, st); why != "" {
		return nil, why
	}
	return st, ""
}

// sameFile reports whether now is still the file then described: the same
// file (device and inode), mode, size, modification time and, where the
// system has one, inode change time, which any write or chmod moves (so
// an in-place rewrite to the same size is caught even if the modification
// time is set back).
func sameFile(then, now os.FileInfo) bool {
	if !os.SameFile(then, now) || then.Mode() != now.Mode() || then.Size() != now.Size() ||
		!then.ModTime().Equal(now.ModTime()) {
		return false
	}
	ct, ok1 := changeTime(then)
	cn, ok2 := changeTime(now)
	if ok1 && ok2 {
		return ct == cn
	}
	return true
}

// errText is a short reason for err without the path it may repeat.
func errText(err error) string {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}
