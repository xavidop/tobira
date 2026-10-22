package ai

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/internal/config"
)

// docFile reads a file relative to the repository root.
func docFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// aiDocs returns the AI pages of the site: ai.md, then each page under ai/,
// in name order.
func aiDocs(t *testing.T) string {
	t.Helper()
	page := docFile(t, "site/src/pages/docs/ai.md")
	pages, err := filepath.Glob(filepath.Join("..", "site/src/pages/docs/ai/*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		page += "\n" + string(b)
	}
	return page
}

// TestAIDocsDefaultModels fails when the default models named by the docs
// differ from the table in providers.go: the "Default models" table of the AI
// page must list exactly the providers config accepts, each with its default,
// and the agent skill must name the same models.
func TestAIDocsDefaultModels(t *testing.T) {
	page := aiDocs(t)
	_, section, ok := strings.Cut(page, "### Default models")
	if !ok {
		t.Fatal("ai.md has no \"### Default models\" heading")
	}
	if i := strings.Index(section, "\n#"); i >= 0 {
		section = section[:i]
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z]+)` \\| `([^`]+)` \\|$")
	documented := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		documented[m[1]] = m[2]
	}
	skill := docFile(t, "skills/tobira/SKILL.md")
	for _, p := range config.AIProviders() {
		want := DefaultModel(p)
		if want == "" {
			t.Fatalf("provider %q has no default model in the code", p)
		}
		if got, ok := documented[p]; !ok {
			t.Errorf("ai.md default models table has no row for provider %q", p)
		} else if got != want {
			t.Errorf("ai.md says the default model of %q is %q, the code says %q", p, got, want)
		}
		if !regexp.MustCompile("(^|[^A-Za-z0-9._-])" + regexp.QuoteMeta(want) + "($|[^A-Za-z0-9_.-]|\\.($|[^0-9A-Za-z]))").MatchString(skill) {
			t.Errorf("SKILL.md does not name the default model %q of %q as an exact token", want, p)
		}
	}
	if len(documented) != len(config.AIProviders()) {
		t.Errorf("ai.md default models table has %d rows, config has %d providers", len(documented), len(config.AIProviders()))
	}
}

// TestAIDocsConfigDefaults fails when the defaults in the AI page's key table
// differ from the ones config applies to an empty ai: section.
func TestAIDocsConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ai: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.AI == nil {
		t.Fatalf("Load: %v, AI = %v", err, cfg)
	}
	want := map[string]string{
		"provider":       cfg.AI.Provider,
		"send_rows":      cfg.AI.SendRows,
		"max_rows":       fmt.Sprint(cfg.AI.MaxRows),
		"max_tool_calls": fmt.Sprint(cfg.AI.MaxToolCalls),
		"context_tokens": fmt.Sprint(cfg.AI.ContextTokens),
	}
	page := aiDocs(t)
	for key, def := range want {
		re := regexp.MustCompile("(?m)^\\| `" + key + "` \\| `([^`]*)` \\|")
		m := re.FindStringSubmatch(page)
		if m == nil {
			t.Errorf("ai.md key table has no default for %s", key)
		} else if m[1] != def {
			t.Errorf("ai.md says the default of ai.%s is %q, the code says %q", key, m[1], def)
		}
	}
	// Every provider the config accepts is named in the page's provider row.
	for _, p := range config.AIProviders() {
		if !strings.Contains(page, "`"+p+"`") {
			t.Errorf("ai.md never names provider %q", p)
		}
	}
}

// TestAIDocsLimits fails when the caps, ranges, key variables and cut-offs the
// AI page states differ from the code.
func TestAIDocsLimits(t *testing.T) {
	page := aiDocs(t)
	has := func(what, text string) {
		t.Helper()
		if !strings.Contains(page, text) {
			t.Errorf("ai.md does not state %s (%q)", what, text)
		}
	}
	// The limits of the numeric keys: the largest value loads, one more does not.
	load := func(key string, v int) error {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("ai:\n  %s: %d\n", key, v)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(path)
		return err
	}
	for _, c := range []struct {
		key    string
		lo, hi int
		row    string
	}{
		{"max_rows", 1, 200, "1 to 200"},
		{"max_tool_calls", 1, 64, "1 to 64"},
		{"context_tokens", 1000, 1000000, "1000 to 1000000"},
	} {
		if load(c.key, c.lo) != nil || load(c.key, c.hi) != nil {
			t.Errorf("%s: %d and %d should load", c.key, c.lo, c.hi)
		}
		if load(c.key, c.lo-1) == nil || load(c.key, c.hi+1) == nil {
			t.Errorf("%s: %d and %d should be refused", c.key, c.lo-1, c.hi+1)
		}
		has(c.key+" range", c.row)
	}
	if tools.MaxRowsCap != 200 {
		t.Errorf("tools.MaxRowsCap = %d, ai.md says 200", tools.MaxRowsCap)
	}
	// The key variables of each provider.
	for name, p := range providers {
		for _, v := range p.keyEnv {
			has("the key variable of "+name, "`"+v+"`")
		}
	}
	// The cut-offs of what is sent.
	has("the value cut", fmt.Sprintf("cut at %d characters", tools.MaxValueRunes))
	has("the history window", fmt.Sprintf("last %d turns", maxHistoryTurns))
	has("the turn cut", fmt.Sprintf("cut at %d characters", maxTurnRunes))
}

// readmeAIAndMCP returns the "## AI & MCP" section of the README.
func readmeAIAndMCP(t *testing.T) string {
	t.Helper()
	_, section, ok := strings.Cut(docFile(t, "README.md"), "\n## AI & MCP\n")
	if !ok {
		t.Fatal("README.md has no \"## AI & MCP\" section")
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	return section
}

// TestReadmeAIAndMCPClaims checks the README's AI & MCP section against the
// code: the provider list in its ai: example is exactly the providers config
// accepts, the MCP "Tools." bullet names every tool and nothing else, the
// mcp.max_rows range is the one config enforces, and mcp.send_rows defaults
// to the value the section says.
func TestReadmeAIAndMCPClaims(t *testing.T) {
	section := readmeAIAndMCP(t)

	m := regexp.MustCompile(`(?m)^\s*provider: \S+\s+# (.*)$`).FindStringSubmatch(section)
	if m == nil {
		t.Fatal("README.md ai: example has no commented provider line")
	}
	listed := regexp.MustCompile(`[a-z]+`).FindAllString(strings.ReplaceAll(m[1], " or ", ", "), -1)
	if want := config.AIProviders(); strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Errorf("README.md lists providers %q; config accepts %q", listed, want)
	}

	_, bullet, ok := strings.Cut(section, "- **Tools.**")
	if !ok {
		t.Fatal("README.md MCP section has no \"- **Tools.**\" bullet")
	}
	if i := strings.Index(bullet, "\n- "); i >= 0 {
		bullet = bullet[:i]
	}
	all := []string{
		tools.NameListConnections, tools.NameListNamespaces, tools.NameListCollections,
		tools.NameDescribe, tools.NameSample, tools.NameQuery, tools.NameExplain,
		tools.NameSimilar, tools.NameExecute,
	}
	named := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(bullet, -1) {
		named[m[1]] = true
	}
	for _, name := range all {
		if !named[name] {
			t.Errorf("README.md MCP Tools bullet does not name the %q tool", name)
		}
		delete(named, name)
	}
	delete(named, "allow_writes") // the config key the bullet explains execute by
	for name := range named {
		t.Errorf("README.md MCP Tools bullet names %q, which is not a tool", name)
	}

	load := func(yaml string) (*config.Config, error) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.Load(path)
	}
	r := regexp.MustCompile(`(?m)^\s*max_rows: \d+\s+# (\d+) to (\d+)$`).FindStringSubmatch(section)
	if r == nil {
		t.Fatal("README.md mcp: example has no \"max_rows: N # lo to hi\" line")
	}
	for v, ok := range map[string]bool{r[1]: true, r[2]: true, "0": false} {
		_, err := load("mcp:\n  expose: [x]\n  max_rows: " + v + "\n")
		if (err == nil) != ok {
			t.Errorf("mcp.max_rows %s: err = %v, README says %s to %s", v, err, r[1], r[2])
		}
	}
	if _, err := load(fmt.Sprintf("mcp:\n  expose: [x]\n  max_rows: %d\n", atoi(t, r[2])+1)); err == nil {
		t.Errorf("mcp.max_rows %d loads, README says at most %s", atoi(t, r[2])+1, r[2])
	}
	cfg, err := load("mcp:\n  expose: [x]\n")
	if err != nil || cfg.MCP == nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(section, "`send_rows` defaults to `"+cfg.MCP.SendRows+"` here") {
		t.Errorf("README.md does not say mcp.send_rows defaults to %q", cfg.MCP.SendRows)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}
