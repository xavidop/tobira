package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/xavidop/tobira/internal/cli"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cliReference returns the CLI reference: cli.md, then each page under cli/,
// in name order.
func cliReference(t *testing.T) string {
	t.Helper()
	return sitePage(t, "cli")
}

// sitePage reads the docs site section site/src/pages/docs/<name>: <name>.md
// if it exists, then each page under <name>/, in name order. A section must
// have at least one of the two.
func sitePage(t *testing.T, name string) string {
	t.Helper()
	base := "../../site/src/pages/docs/" + name
	var doc strings.Builder
	if b, err := os.ReadFile(base + ".md"); err == nil {
		doc.Write(b)
	}
	pages, err := filepath.Glob(base + "/*.md")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(pages)
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		doc.WriteString("\n")
		doc.Write(b)
	}
	if doc.Len() == 0 {
		t.Fatalf("the docs site has no %s.md and no %s/*.md", name, name)
	}
	return doc.String()
}

// isBuiltin reports the commands the user docs do not list: those cobra
// adds itself (documented in one sentence) and hidden developer commands
// (`tobira ai dev`, documented in CONTRIBUTING.md).
func isBuiltin(c *cobra.Command) bool {
	return c.Name() == "help" || c.Name() == "completion" || c.Hidden
}

func newRoot() *cobra.Command {
	return cli.NewRoot(cli.BuildInfo{}, cli.Streams{}, cli.Deps{})
}

// docSections splits the CLI reference into the text before the first
// "## tobira <name>" heading and one body per such heading.
func docSections(doc string) (intro string, sections map[string]string) {
	heading := regexp.MustCompile(`(?m)^## tobira (\S+)\s*$`)
	idx := heading.FindAllStringSubmatchIndex(doc, -1)
	sections = map[string]string{}
	intro = doc
	if len(idx) > 0 {
		intro = doc[:idx[0][0]]
	}
	anyHeading := regexp.MustCompile(`(?m)^## `)
	for _, m := range idx {
		end := len(doc)
		// A section runs to the next "## " heading, a tobira command or not.
		if next := anyHeading.FindStringIndex(doc[m[1]:]); next != nil {
			end = m[1] + next[0]
		}
		sections[doc[m[2]:m[3]]] = doc[m[1]:end]
	}
	return intro, sections
}

func localFlags(c *cobra.Command) []string {
	var names []string
	c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" {
			names = append(names, "--"+f.Name)
		}
	})
	return names
}

// TestDocsCoverCommandsAndFlags fails when the CLI reference and the cobra
// command tree disagree: a command or flag the code has but the page lacks,
// or a "## tobira <name>" heading or flags-table row the code does not have.
func TestDocsCoverCommandsAndFlags(t *testing.T) {
	doc := cliReference(t)
	intro, sections := docSections(doc)
	root := newRoot()

	// Persistent flags and the root's own flags are in the intro.
	for _, f := range []string{"--config", "--debug", "--read-only"} {
		if !strings.Contains(intro, f) {
			t.Errorf("cli.md intro does not mention %s", f)
		}
	}

	known := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		if isBuiltin(c) {
			continue
		}
		known[c.Name()] = c
		body, ok := sections[c.Name()]
		if !ok {
			t.Errorf("cli.md has no \"## tobira %s\" heading", c.Name())
			continue
		}
		check := func(path string, cmd *cobra.Command) {
			if !strings.Contains(body, "tobira "+path) {
				t.Errorf("cli.md section %q does not mention \"tobira %s\"", c.Name(), path)
			}
			// A subcommand's flags must be on its own usage line, not just
			// somewhere in the section.
			scope := body
			if cmd != c {
				scope = usageLine(body, path)
				if scope == "" {
					t.Errorf("cli.md section %q has no usage line starting \"tobira %s\"", c.Name(), path)
				}
			}
			for _, f := range localFlags(cmd) {
				if !strings.Contains(scope, f) {
					t.Errorf("cli.md section %q does not mention %s of \"tobira %s\"", c.Name(), f, path)
				}
			}
		}
		check(c.Name(), c)
		for _, sub := range c.Commands() {
			if isBuiltin(sub) {
				continue
			}
			check(c.Name()+" "+sub.Name(), sub)
		}
	}
	for name := range sections {
		if _, ok := known[name]; !ok {
			t.Errorf("cli.md documents \"tobira %s\", which does not exist", name)
		}
	}

	// Every flag in a flags-table row must exist on that command or be global.
	row := regexp.MustCompile("(?m)^\\| `(?:-\\w, )?(--[a-z-]+)")
	for name, body := range sections {
		c := known[name]
		if c == nil {
			continue
		}
		have := map[string]bool{"--config": true, "--debug": true}
		c.Flags().VisitAll(func(f *pflag.Flag) { have["--"+f.Name] = true })
		for _, m := range row.FindAllStringSubmatch(body, -1) {
			if !have[m[1]] {
				t.Errorf("cli.md section %q lists %s, which \"tobira %s\" does not have", name, m[1], name)
			}
		}
		// The usage lines of subcommands may only show flags the subcommand has.
		for _, sub := range c.Commands() {
			if isBuiltin(sub) {
				continue
			}
			subHave := map[string]bool{"--config": true, "--debug": true}
			sub.Flags().VisitAll(func(f *pflag.Flag) { subHave["--"+f.Name] = true })
			c.PersistentFlags().VisitAll(func(f *pflag.Flag) { subHave["--"+f.Name] = true })
			for _, f := range flagRef.FindAllString(usageLine(body, name+" "+sub.Name()), -1) {
				if !subHave[f] {
					t.Errorf("cli.md usage of \"tobira %s %s\" shows %s, which it does not have", name, sub.Name(), f)
				}
			}
		}
	}
}

var flagRef = regexp.MustCompile(`--[a-z][a-z0-9-]*`)

// usageLine returns the line of body that starts "tobira <path>" followed by
// a space or the end of the line, or "" when there is none.
func usageLine(body, path string) string {
	for _, line := range strings.Split(body, "\n") {
		if line == "tobira "+path || strings.HasPrefix(line, "tobira "+path+" ") {
			return line
		}
	}
	return ""
}

// TestUserDocsNameEveryCommand keeps the README and the agent skill in step
// with the command tree: each must show every command, and the README's CLI
// block every connections subcommand.
func TestUserDocsNameEveryCommand(t *testing.T) {
	root := newRoot()
	for _, file := range []string{"README.md", "skills/tobira/SKILL.md"} {
		text := repoFile(t, file)
		for _, c := range root.Commands() {
			if isBuiltin(c) {
				continue
			}
			if !strings.Contains(text, "tobira "+c.Name()) {
				t.Errorf("%s does not mention \"tobira %s\"", file, c.Name())
			}
			for _, sub := range c.Commands() {
				if isBuiltin(sub) {
					continue
				}
				// The README lists subcommands as "a|b|c", checked strictly
				// below; elsewhere each must follow its command by name.
				if file == "README.md" {
					continue
				}
				if !regexp.MustCompile(`tobira ` + regexp.QuoteMeta(c.Name()) + `\s+` + regexp.QuoteMeta(sub.Name()) + `\b`).MatchString(text) {
					t.Errorf("%s does not mention \"tobira %s %s\"", file, c.Name(), sub.Name())
				}
			}
		}
	}
	// README's CLI block lists the connections subcommands in one line.
	readme := repoFile(t, "README.md")
	line := regexp.MustCompile(`(?m)^tobira connections (\S+\|\S+)`).FindStringSubmatch(readme)
	if line == nil {
		t.Fatal("README.md has no \"tobira connections a|b|c\" line in its CLI block")
	}
	listed := map[string]bool{}
	for _, s := range strings.Split(line[1], "|") {
		listed[s] = true
	}
	for _, c := range root.Commands() {
		if c.Name() != "connections" {
			continue
		}
		for _, sub := range c.Commands() {
			if !isBuiltin(sub) && !listed[sub.Name()] {
				t.Errorf("README.md CLI block does not list connections %s", sub.Name())
			}
		}
		for s := range listed {
			found := false
			for _, sub := range c.Commands() {
				found = found || sub.Name() == s
			}
			if !found {
				t.Errorf("README.md CLI block lists connections %s, which does not exist", s)
			}
		}
	}
}

// TestHiddenCommandsAreDocumented keeps the docs honest about the developer
// commands `--help` does not list: each hidden command with a Run function
// must appear by its full name in CONTRIBUTING.md, the developer docs, and a
// hidden flag of a listed command must appear in the CLI reference. The user
// CLI reference does not document developer commands.
func TestHiddenCommandsAreDocumented(t *testing.T) {
	doc := cliReference(t)
	section := repoFile(t, "CONTRIBUTING.md")
	if strings.Contains(doc, "## Hidden commands") {
		t.Error("cli.md documents hidden developer commands; they belong in CONTRIBUTING.md")
	}
	var walk func(prefix string, c *cobra.Command, hidden bool)
	walk = func(prefix string, c *cobra.Command, hidden bool) {
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			path := prefix + sub.Name()
			// A visible child of a hidden group is just as unlisted by --help.
			subHidden := hidden || sub.Hidden
			where := doc
			if subHidden {
				where = section
			}
			if subHidden && sub.Runnable() && !regexp.MustCompile(`tobira `+regexp.QuoteMeta(path)+`\b`).MatchString(where) {
				t.Errorf("CONTRIBUTING.md does not mention the hidden command \"tobira %s\"", path)
			}
			// A hidden flag is not in --help, so the reference must name it.
			sub.Flags().VisitAll(func(f *pflag.Flag) {
				if f.Hidden && !strings.Contains(where, "--"+f.Name) {
					t.Errorf("the docs do not mention the hidden flag --%s of \"tobira %s\"", f.Name, path)
				}
			})
			walk(path+" ", sub, subHidden)
		}
	}
	walk("", newRoot(), false)
}

// TestMCPSetupLinesUseRealFlags keeps the copy-paste MCP setup honest: the
// README must carry the Claude Code one-liner, and every "tobira mcp ..."
// command line in the README, the MCP page and the agent skill may use only
// flags that `tobira mcp` has.
func TestMCPSetupLinesUseRealFlags(t *testing.T) {
	var mcpCmd *cobra.Command
	for _, c := range newRoot().Commands() {
		if c.Name() == "mcp" {
			mcpCmd = c
		}
	}
	if mcpCmd == nil {
		t.Fatal("no mcp command")
	}
	have := map[string]bool{"--config": true, "--debug": true}
	mcpCmd.Flags().VisitAll(func(f *pflag.Flag) { have["--"+f.Name] = true })

	oneLiner := regexp.MustCompile("(?m)^claude mcp add tobira -- tobira mcp .*$")
	readme := repoFile(t, "README.md")
	if !oneLiner.MatchString(readme) {
		t.Error("README.md has no \"claude mcp add tobira -- tobira mcp ...\" one-liner")
	}
	if !oneLiner.MatchString(sitePage(t, "mcp")) {
		t.Error("mcp.md has no \"claude mcp add tobira -- tobira mcp ...\" one-liner")
	}

	cmdLine := regexp.MustCompile("(?m)^(?:claude mcp add [^\\n]*-- )?(tobira mcp\\b[^\\n#`]*)")
	docs := map[string]string{
		"README.md":              readme,
		"the MCP page":           sitePage(t, "mcp"),
		"skills/tobira/SKILL.md": repoFile(t, "skills/tobira/SKILL.md"),
	}
	for file, text := range docs {
		n := 0
		for _, m := range cmdLine.FindAllStringSubmatch(text, -1) {
			n++
			for _, f := range flagRef.FindAllString(m[1], -1) {
				if !have[f] {
					t.Errorf("%s: %q uses %s, which \"tobira mcp\" does not have", file, strings.TrimSpace(m[1]), f)
				}
			}
		}
		if n == 0 {
			t.Errorf("%s has no \"tobira mcp ...\" command line", file)
		}
	}
}
