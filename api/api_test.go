package api_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/xavidop/tobira/api"
	"github.com/xavidop/tobira/driver"
)

// notReexported are driver's exported names api leaves out on purpose: the
// process-wide registry belongs to the host.
var notReexported = map[string]bool{
	"Default":    true,
	"Register":   true,
	"Open":       true,
	"OpenConfig": true,
	"Drivers":    true,
}

// exported returns the exported top-level names declared in the non-test Go
// files of dir, and the import paths those files use.
func exported(t *testing.T, dir string) (names map[string]bool, imports map[string]bool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	names, imports = map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			imports[p] = true
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					names[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							names[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								names[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return names, imports
}

// TestCoversTheDriverSurface fails when driver gains an exported name that
// api neither re-exports nor lists in notReexported, so the stable surface
// cannot silently fall behind.
func TestCoversTheDriverSurface(t *testing.T) {
	drv, _ := exported(t, "../driver")
	a, _ := exported(t, ".")
	var missing, extra []string
	for n := range drv {
		if !a[n] && !notReexported[n] {
			missing = append(missing, n)
		}
	}
	for n := range a {
		if !drv[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("api does not re-export these driver names: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("api declares names driver does not have: %v", extra)
	}
	for n := range notReexported {
		if a[n] {
			t.Errorf("api re-exports %s, which is listed as left out", n)
		}
	}
}

func TestImportsOnlyDriverAndStdlib(t *testing.T) {
	_, imports := exported(t, ".")
	for p := range imports {
		first, _, _ := strings.Cut(p, "/")
		if p != "github.com/xavidop/tobira/driver" && strings.Contains(first, ".") {
			t.Errorf("api imports %s; it may import only driver and the standard library", p)
		}
	}
}

// memDriver is a driver written against api alone.
type memDriver struct{}

func (memDriver) Info() api.Info {
	return api.Info{Name: "mem", Kind: api.KindOther, Capabilities: []api.Capability{api.CapReadOnly}}
}

func (memDriver) Open(context.Context, api.ConnConfig) (api.Conn, error) {
	return nil, api.Wrap(api.ErrUnsupported, "mem", "open", errors.New("no connections"))
}

func TestAliasesAreTheDriverTypes(t *testing.T) {
	var d driver.Driver = memDriver{} // an api.Driver is a driver.Driver
	if !driver.HasCapability(d.Info(), driver.CapReadOnly) {
		t.Fatal("CapReadOnly did not survive the alias")
	}
	_, err := d.Open(context.Background(), driver.ConnConfig{})
	if !errors.Is(err, driver.ErrUnsupported) || api.KindName(err) != "unsupported" {
		t.Fatalf("err = %v, want an unsupported driver error", err)
	}
	show := func(v driver.Value) string { return v.String() } // takes an api.Value as is
	if got := show(api.ObjectValue(api.Member{Key: "k", Value: api.Int(1)})); got != `{"k":1}` {
		t.Fatalf("value = %s", got)
	}
	rs := api.NewSliceResultSet([]api.Column{{Name: "n", Type: api.TypeInt}}, []api.Row{{api.Int(1)}}, api.Stats{})
	rows, _, err := driver.Collect(context.Background(), rs, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Collect = %v, %v", rows, err)
	}
	if b, ok := api.As[api.Browser](rs); ok || b != nil {
		t.Fatal("a slice result set is not a Browser")
	}
}
