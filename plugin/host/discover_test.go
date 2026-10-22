package host

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestValidName(t *testing.T) {
	long := strings.Repeat("a", 63)
	cases := []struct {
		file, name string
		ok         bool
	}{
		{"tobira-driver-custom", "custom", true},
		{"tobira-driver-my-db2", "my-db2", true},
		{"tobira-driver-0", "0", true},
		{"tobira-driver-" + long, long, true},
		{"tobira-driver-" + long + "a", "", false},
		{"tobira-driver-", "", false},
		{"tobira-driver--x", "", false},
		{"tobira-driver-Custom", "", false},
		{"tobira-driver-my_db", "", false},
		{"tobira-driver-a.b", "", false},
		{"tobira-driver-../x", "", false},
		{"tobira-driver-a b", "", false},
		{"tobira-drivercustom", "", false},
		{"xtobira-driver-custom", "", false},
		{"tobira-driver-custom\n", "", false},
	}
	for _, c := range cases {
		name, ok := ValidName(c.file)
		if name != c.name || ok != c.ok {
			t.Errorf("ValidName(%q) = %q, %v; want %q, %v", c.file, name, ok, c.name, c.ok)
		}
	}
}

func TestValidNameExeSuffix(t *testing.T) {
	name, ok := validName("tobira-driver-custom.exe", "windows")
	if !ok || name != "custom" {
		t.Errorf("windows .exe: got %q, %v", name, ok)
	}
	if name, ok := validName("tobira-driver-custom.EXE", "windows"); !ok || name != "custom" {
		t.Errorf("windows .EXE: got %q, %v", name, ok)
	}
	if _, ok := validName("tobira-driver-custom.exe", "linux"); ok {
		t.Error("a .exe suffix must not be stripped outside Windows")
	}
}

// writeExec writes an executable file (or a plain one, when mode lacks x).
func writeFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func names(ps []Plugin) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func find(ps []Plugin, name string) (Plugin, bool) {
	i := slices.IndexFunc(ps, func(p Plugin) bool { return p.Name == name })
	if i < 0 {
		return Plugin{}, false
	}
	return ps[i], true
}

func TestDiscoverFiltersNamesAndFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits and symlinks are unix semantics")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tobira-driver-good"), 0o755)
	writeFile(t, filepath.Join(dir, "tobira-driver-Bad"), 0o755)
	writeFile(t, filepath.Join(dir, "tobira-driver-noexec"), 0o644)
	writeFile(t, filepath.Join(dir, "unrelated"), 0o755)
	if err := os.Mkdir(filepath.Join(dir, "tobira-driver-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := Discover([]string{dir})
	if !slices.Equal(names(got), []string{"good"}) {
		t.Fatalf("Discover = %v, want [good]", names(got))
	}
	p := got[0]
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "tobira-driver-good"))
	if p.Path != want || p.Dir != dir {
		t.Errorf("plugin = %+v, want Path %q Dir %q", p, want, dir)
	}
}

func TestDiscoverResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	real := t.TempDir()
	dir := t.TempDir()
	target := filepath.Join(real, "some-binary")
	writeFile(t, target, 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "tobira-driver-linked")); err != nil {
		t.Fatal(err)
	}
	// A link to a directory, a dangling link and a link to a non-executable
	// file are all skipped.
	if err := os.Symlink(real, filepath.Join(dir, "tobira-driver-todir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "missing"), filepath.Join(dir, "tobira-driver-dangling")); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(real, "plain")
	writeFile(t, plain, 0o644)
	if err := os.Symlink(plain, filepath.Join(dir, "tobira-driver-plain")); err != nil {
		t.Fatal(err)
	}
	// A link whose own name is invalid is skipped even if the target's is valid.
	writeFile(t, filepath.Join(real, "tobira-driver-target"), 0o755)
	if err := os.Symlink(filepath.Join(real, "tobira-driver-target"), filepath.Join(dir, "Tobira-Driver-X")); err != nil {
		t.Fatal(err)
	}

	got := Discover([]string{dir})
	if !slices.Equal(names(got), []string{"linked"}) {
		t.Fatalf("Discover = %v, want [linked]", names(got))
	}
	want, _ := filepath.EvalSymlinks(target)
	if got[0].Path != want {
		t.Errorf("Path = %q, want the resolved target %q", got[0].Path, want)
	}
}

func TestDiscoverFirstDirWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
	plugins, path1, path2 := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(plugins, "tobira-driver-dup"), 0o755)
	writeFile(t, filepath.Join(path1, "tobira-driver-dup"), 0o755)
	writeFile(t, filepath.Join(path1, "tobira-driver-only-path"), 0o755)
	writeFile(t, filepath.Join(path2, "tobira-driver-only-path"), 0o755)

	got := Discover([]string{plugins, path1, path2})
	slices.SortFunc(got, func(a, b Plugin) int { return strings.Compare(a.Name, b.Name) })
	if !slices.Equal(names(got), []string{"dup", "only-path"}) {
		t.Fatalf("Discover = %v", names(got))
	}
	if p, _ := find(got, "dup"); p.Dir != plugins {
		t.Errorf("dup came from %q, want the plugins dir %q", p.Dir, plugins)
	}
	if p, _ := find(got, "only-path"); p.Dir != path1 {
		t.Errorf("only-path came from %q, want the first PATH entry %q", p.Dir, path1)
	}
}

func TestDiscoverSkipsRelativeAndMissingDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tobira-driver-here"), 0o755)
	t.Chdir(dir)
	got := Discover([]string{"", ".", "relative/dir", filepath.Join(dir, "missing")})
	if len(got) != 0 {
		t.Fatalf("Discover = %v, want nothing: relative PATH entries are never searched", names(got))
	}
}

func TestSearchDirs(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := SearchDirs([]string{"/p", ""}, "/a"+sep+sep+"/p"+sep+"/b")
	want := []string{"/p", "/a", "/b"}
	if !slices.Equal(got, want) {
		t.Errorf("SearchDirs = %v, want %v", got, want)
	}
}

func TestDiscoverAllReportsShadowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
	plugins, path1, path2 := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(plugins, "tobira-driver-dup"), 0o755)
	writeFile(t, filepath.Join(path1, "tobira-driver-dup"), 0o755)
	writeFile(t, filepath.Join(path2, "tobira-driver-dup"), 0o755)
	writeFile(t, filepath.Join(path2, "tobira-driver-alone"), 0o755)
	writeFile(t, filepath.Join(path2, "tobira-driver-bad"), 0o644) // not executable: skipped, not shadowed

	found, shadowed := DiscoverAll([]string{plugins, path1, path2}, nil)
	slices.SortFunc(found, func(a, b Plugin) int { return strings.Compare(a.Name, b.Name) })
	if !slices.Equal(names(found), []string{"alone", "dup"}) {
		t.Fatalf("found = %v", names(found))
	}
	if len(shadowed) != 2 {
		t.Fatalf("shadowed = %+v, want the two PATH copies of dup", shadowed)
	}
	for i, dir := range []string{path1, path2} {
		if shadowed[i].Name != "dup" || shadowed[i].Dir != dir {
			t.Errorf("shadowed[%d] = %+v, want dup from %s", i, shadowed[i], dir)
		}
	}
	again := Discover([]string{plugins, path1, path2})
	slices.SortFunc(again, func(a, b Plugin) int { return strings.Compare(a.Name, b.Name) })
	if !slices.Equal(names(again), names(found)) {
		t.Error("Discover and DiscoverAll disagree on the plugins found")
	}
}

func TestUnsafeDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are unix semantics")
	}
	dir := t.TempDir()
	if why := UnsafeDir(dir); why != "" {
		t.Errorf("UnsafeDir(0700 dir) = %q, want \"\"", why)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if why := UnsafeDir(dir); why == "" {
		t.Error("UnsafeDir(world-writable dir) = \"\"")
	}
	if err := os.Chmod(dir, 0o1777); err != nil {
		t.Fatal(err)
	}
	if why := UnsafeDir(dir); why == "" {
		t.Error("UnsafeDir(sticky world-writable dir) = \"\": others could still add plugins to it")
	}
	if why := UnsafeDir(filepath.Join(dir, "missing")); why == "" {
		t.Error("UnsafeDir(missing dir) = \"\"")
	}
}

func TestUnsafeDirRefusesGroupWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are unix semantics")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if why := UnsafeDir(dir); why != "group-writable" {
		t.Errorf("UnsafeDir(0770 dir) = %q, want group-writable", why)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if why := UnsafeDir(dir); why != "" {
		t.Errorf("UnsafeDir(0755 dir of this user) = %q, want \"\"", why)
	}
}

func TestDiscoverRefusesWorldWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tobira-driver-ok"), 0o755)
	writeFile(t, filepath.Join(dir, "tobira-driver-open"), 0o757)

	// A world-writable directory without the sticky bit taints its files.
	loose := t.TempDir()
	writeFile(t, filepath.Join(loose, "tobira-driver-loose"), 0o755)
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	// With the sticky bit and owned by the user (like /tmp), it does not.
	sticky := t.TempDir()
	writeFile(t, filepath.Join(sticky, "tobira-driver-sticky"), 0o755)
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	// A symlink in a safe dir pointing into the loose dir is refused too:
	// the target's directory is what counts.
	if err := os.Symlink(filepath.Join(loose, "tobira-driver-loose"), filepath.Join(dir, "tobira-driver-viaink")); err != nil {
		t.Fatal(err)
	}

	got := names(Discover([]string{dir, loose, sticky}))
	slices.Sort(got)
	if want := []string{"ok", "sticky"}; !slices.Equal(got, want) {
		t.Errorf("Discover = %v, want %v", got, want)
	}
}

func TestNormalizeInfo(t *testing.T) {
	a := &adapter{h: &Host{log: slog.New(slog.DiscardHandler)}, plugin: Plugin{Name: "x"}, routed: true}
	info := a.normalize(driver.Info{
		Name: "other", Kind: "weird-kind", Schemes: []string{"MyDB", "mydb", "", "x"},
		Capabilities: []driver.Capability{"browser", "junk", "browser", "readonly"},
	})
	if info.Name != "x" || info.Kind != driver.KindOther {
		t.Errorf("name/kind = %q/%q", info.Name, info.Kind)
	}
	if want := []string{"x", "mydb"}; !slices.Equal(info.Schemes, want) {
		t.Errorf("schemes = %v, want %v", info.Schemes, want)
	}
	if want := []driver.Capability{"browser", "readonly"}; !slices.Equal(info.Capabilities, want) {
		t.Errorf("caps = %v, want %v", info.Capabilities, want)
	}
	if got := a.normalize(driver.Info{Kind: driver.KindVector}).Kind; got != driver.KindVector {
		t.Errorf("a known kind became %q", got)
	}
}
