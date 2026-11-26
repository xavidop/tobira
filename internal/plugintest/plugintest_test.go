package plugintest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot is the repository root, from this package's directory.
var repoRoot = filepath.Join("..", "..")

// goCmd runs the go command in dir outside the workspace, as the build does.
func goCmd(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	return cmd.CombinedOutput()
}

// behindModule copies the test plugin into a new directory with a
// requirement missing from its go.mod, which is how the module looks to the
// go command after the modules it replaces have moved on without it. It
// returns the directory and the go.sum files of the replaced modules.
func behindModule(t *testing.T) (dir string, sums []string) {
	t.Helper()
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "plugin", "host", "testdata", "plugin")
	dir = t.TempDir()
	for _, name := range []string{"go.mod", "go.sum", "main.go"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sdk := filepath.Join(root, "plugin", "sdk")
	if out, err := goCmd(dir, "mod", "edit",
		"-replace=github.com/xavidop/tobira="+root,
		"-replace=github.com/xavidop/tobira/plugin/sdk="+sdk,
		"-droprequire=google.golang.org/genproto/googleapis/rpc",
	); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}
	return dir, []string{filepath.Join(root, "go.sum"), filepath.Join(sdk, "go.sum")}
}

// A go.mod that is behind the modules it replaces stops a plain build, and
// build compiles the module anyway without writing to it.
func TestBuildFollowsTheReplacedModules(t *testing.T) {
	dir, sums := behindModule(t)
	if out, err := goCmd(dir, "build", "-o", filepath.Join(t.TempDir(), "plain"), "."); err == nil {
		t.Fatalf("a plain build of the module passed, so it is not behind:\n%s", out)
	}
	modBefore, sumBefore := readModuleFiles(t, dir)

	bin := filepath.Join(t.TempDir(), "plugin")
	if err := build(dir, sums, bin); err != nil {
		t.Fatalf("build: %v", err)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Size() == 0 {
		t.Fatalf("no binary at %s: %v", bin, err)
	}
	modAfter, sumAfter := readModuleFiles(t, dir)
	if !bytes.Equal(modBefore, modAfter) {
		t.Errorf("build rewrote go.mod:\n%s", modAfter)
	}
	if !bytes.Equal(sumBefore, sumAfter) {
		t.Error("build rewrote go.sum")
	}
}

// Build compiles the test plugin in the repository and leaves its module
// files as they are.
func TestBuildCompilesTheTestPlugin(t *testing.T) {
	dir := filepath.Join(repoRoot, "plugin", "host", "testdata", "plugin")
	modBefore, sumBefore := readModuleFiles(t, dir)

	bin := filepath.Join(t.TempDir(), "plugin")
	if err := Build(repoRoot, bin); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Size() == 0 {
		t.Fatalf("no binary at %s: %v", bin, err)
	}
	modAfter, sumAfter := readModuleFiles(t, dir)
	if !bytes.Equal(modBefore, modAfter) || !bytes.Equal(sumBefore, sumAfter) {
		t.Error("Build rewrote the test plugin's module files")
	}
}

func readModuleFiles(t *testing.T, dir string) (mod, sum []byte) {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err = os.ReadFile(filepath.Join(dir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	return mod, sum
}
