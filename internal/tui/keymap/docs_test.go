package keymap

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the generated keymaps page")

// docsPath is the generated page, relative to this package.
var docsPath = filepath.Join("..", "..", "..", "site", "src", "pages", "docs", "keymaps.md")

func TestGenerateDocs(t *testing.T) {
	if !*update {
		t.Skip("run with -update to rewrite " + docsPath)
	}
	if err := os.WriteFile(docsPath, []byte(Markdown()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKeymapDocsUpToDate(t *testing.T) {
	got, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("%v (generate it with: go test ./internal/tui/keymap -run TestGenerateDocs -update)", err)
	}
	if string(got) != Markdown() {
		t.Error("keymaps.md is stale; run: go test ./internal/tui/keymap -run TestGenerateDocs -update")
	}
}
