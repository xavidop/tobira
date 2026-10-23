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
)

// readmeSections are the "## " headings of the README, in order.
var readmeSections = []string{
	"Why", "Install", "Quick start", "Features", "Drivers", "AI & MCP", "CLI",
	"Agent skill", "Documentation", "Contributing", "License",
}

// readmeSection returns the body of the "## <name>" section of the README.
func readmeSection(t *testing.T, readme, name string) string {
	t.Helper()
	return mdSection(t, "README.md", readme, name)
}

// mdSection returns the body of the "## <name>" section of text, the
// contents of file.
func mdSection(t *testing.T, file, text, name string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, "\n## "+name+"\n")
	if !ok {
		t.Fatalf("%s has no \"## %s\" section", file, name)
	}
	if i := strings.Index(after, "\n## "); i >= 0 {
		after = after[:i]
	}
	return after
}

// outsideFences drops the lines inside ``` code fences.
func outsideFences(text string) []string {
	var out []string
	fence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "```") {
			fence = !fence
			continue
		}
		if !fence {
			out = append(out, line)
		}
	}
	return out
}

// TestReadmeShape keeps the README in its fixed shape: the logo,
// "# 扉 tobira", the tagline, an italic one-liner, the three badges in order,
// then exactly the sections of readmeSections in that order, with the demo
// GIF in Quick start.
func TestReadmeShape(t *testing.T) {
	readme := repoFile(t, "README.md")
	lines := outsideFences(readme)

	// The header, in order.
	header := []struct {
		what string
		re   *regexp.Regexp
	}{
		{"logo", regexp.MustCompile(`^<img src="site/public/favicon\.svg"`)},
		{"title", regexp.MustCompile(`^# 扉 (&nbsp;)?tobira$`)},
		{"tagline", regexp.MustCompile(`^### Every database, one terminal\.$`)},
		{"italic one-liner", regexp.MustCompile(`^\*[^*].*$`)},
		{"Go Reference badge", regexp.MustCompile(`^\[!\[Go Reference\]`)},
		{"CI badge", regexp.MustCompile(`^\[!\[CI\]`)},
		{"MIT badge", regexp.MustCompile(`^\[!\[License: MIT\]`)},
	}
	next := 0
	firstSection := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			firstSection = i
			break
		}
		if next < len(header) && header[next].re.MatchString(line) {
			next++
		}
	}
	if next < len(header) {
		t.Errorf("README.md header: missing or out of order from %q (want logo, title, tagline, italic one-liner, then the Go Reference, CI and MIT badges)", header[next].what)
	}
	if firstSection < 0 {
		t.Fatal("README.md has no \"## \" section")
	}
	if _, err := os.Stat("../../site/public/favicon.svg"); err != nil {
		t.Errorf("the README logo: %v", err)
	}

	var got []string
	for _, line := range lines {
		if name, ok := strings.CutPrefix(line, "## "); ok {
			got = append(got, name)
		}
	}
	if !slices.Equal(got, readmeSections) {
		t.Errorf("README.md sections:\n got  %q\n want %q", got, readmeSections)
	}

	// The intro, before the first section, shows the vhs demo, and the GIF exists.
	intro, _, _ := strings.Cut(readme, "\n## ")
	if !strings.Contains(intro, `src="site/public/demo.gif"`) {
		t.Error("README.md does not show site/public/demo.gif before its first section")
	}
	if _, err := os.Stat("../../site/public/demo.gif"); err != nil {
		t.Errorf("the README demo: %v", err)
	}
}

// TestReadmeInstallMatchesTheRelease keeps the Install section true to
// .goreleaser.yaml: brew, Scoop and the releases page are named, a cask is
// called macOS only, each package format the release builds is named, every
// platform the archives build is named, and `go install` is not offered (the
// CLI module replaces its sibling modules with the checkout, which
// `go install pkg@version` refuses).
func TestReadmeInstallMatchesTheRelease(t *testing.T) {
	install := readmeSection(t, repoFile(t, "README.md"), "Install")
	release := repoFile(t, ".goreleaser.yaml")
	for _, want := range []string{"brew install xavidop/tap/tobira", "scoop install tobira", "releases"} {
		if !strings.Contains(install, want) {
			t.Errorf("README.md Install does not mention %q", want)
		}
	}
	for _, page := range []string{"README.md", "site/src/pages/docs/install.md", "site/src/pages/docs/index.md"} {
		if strings.Contains(repoFile(t, page), "go install github.com/xavidop/tobira") {
			t.Errorf("%s offers go install, which cannot build the CLI module", page)
		}
	}
	if strings.Contains(release, "homebrew_casks:") && !strings.Contains(install, "macOS only") {
		t.Error("README.md Install: the release publishes a Homebrew cask, so brew must be called macOS only")
	}
	if strings.Contains(release, "scoops:") != strings.Contains(install, "Scoop") {
		t.Error("README.md Install and .goreleaser.yaml disagree about Scoop")
	}
	formats := regexp.MustCompile(`(?s)nfpms:.*?formats: \[([^\]]*)\]`).FindStringSubmatch(release)
	if strings.Contains(release, "nfpms:") && formats == nil {
		t.Fatal(".goreleaser.yaml has nfpms but no formats list")
	}
	if formats != nil {
		for _, f := range strings.Split(formats[1], ",") {
			if f = strings.TrimSpace(f); !strings.Contains(install, "."+f) {
				t.Errorf("README.md Install does not mention the .%s package the release builds", f)
			}
		}
	}
	goos := regexp.MustCompile(`(?m)^\s*goos: \[([^\]]*)\]`).FindStringSubmatch(release)
	if goos == nil {
		t.Fatal(".goreleaser.yaml has no goos list")
	}
	names := map[string]string{"linux": "Linux", "darwin": "macOS", "windows": "Windows"}
	for _, os := range strings.Split(goos[1], ",") {
		os = strings.TrimSpace(os)
		if !strings.Contains(install, names[os]) {
			t.Errorf("README.md Install does not name %s, which the release builds", names[os])
		}
	}
}

// TestReadmeCLIBlockMatchesTheCommands checks the README's CLI block against
// the cobra tree: every line names a real command, every command has a line,
// every "a|b|c" list is exactly that command's subcommands, and every flag
// shown exists on the command or one of its subcommands.
func TestReadmeCLIBlockMatchesTheCommands(t *testing.T) {
	section := readmeSection(t, repoFile(t, "README.md"), "CLI")
	_, block, ok := strings.Cut(section, "```text\n")
	if !ok {
		t.Fatal("README.md CLI section has no ```text block")
	}
	block, _, _ = strings.Cut(block, "```")

	root := newRoot()
	cmds := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		if !isBuiltin(c) {
			cmds[c.Name()] = c
		}
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		// A line is "<command and arguments>  <description>": two or more
		// spaces end the command column.
		command := regexp.MustCompile(`\s{2,}`).Split(line, 2)[0]
		fields := strings.Fields(command)
		if len(fields) == 0 || fields[0] != "tobira" {
			t.Errorf("README.md CLI line %q does not start with \"tobira\"", line)
			continue
		}
		if len(fields) == 1 || !regexp.MustCompile(`^[a-z]`).MatchString(fields[1]) {
			seen[""] = true // the root command
			continue
		}
		name := fields[1]
		c := cmds[name]
		if c == nil {
			t.Errorf("README.md CLI block lists \"tobira %s\", which does not exist", name)
			continue
		}
		seen[name] = true
		if len(fields) > 2 && strings.Contains(fields[2], "|") && !strings.HasPrefix(fields[2], "<") {
			var subs []string
			for _, s := range c.Commands() {
				if !isBuiltin(s) {
					subs = append(subs, s.Name())
				}
			}
			listed := strings.Split(fields[2], "|")
			slices.Sort(subs)
			slices.Sort(listed)
			if !slices.Equal(listed, subs) {
				t.Errorf("README.md CLI block lists tobira %s %s; the subcommands are %s", name, fields[2], strings.Join(subs, "|"))
			}
		}
		have := map[string]bool{"--config": true, "--debug": true, "--read-only": true}
		var add func(*cobra.Command)
		add = func(c *cobra.Command) {
			c.Flags().VisitAll(func(f *pflag.Flag) { have["--"+f.Name] = true })
			for _, s := range c.Commands() {
				add(s)
			}
		}
		add(c)
		for _, f := range flagRef.FindAllString(line, -1) {
			if !have[f] {
				t.Errorf("README.md CLI line for \"tobira %s\" shows %s, which it does not have", name, f)
			}
		}
	}
	if !seen[""] {
		t.Error("README.md CLI block has no line for plain \"tobira\"")
	}
	for name := range cmds {
		if !seen[name] {
			t.Errorf("README.md CLI block has no line for \"tobira %s\"", name)
		}
	}
}

// githubSlug is GitHub's anchor for a Markdown heading.
func githubSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestReadmeLinksResolve checks every link of the README that this
// repository can answer: a relative link names a file or directory that
// exists (and, into a Markdown file, a heading that exists), and a link into
// the docs site names a page the site has. Other links are not fetched.
func TestReadmeLinksResolve(t *testing.T) {
	readme := repoFile(t, "README.md")
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	site := regexp.MustCompile(`^https://xavidop\.github\.io/tobira/docs/([a-z0-9/-]*?)/?(#.*)?$`)
	for _, m := range link.FindAllStringSubmatch(readme, -1) {
		target := m[1]
		if sm := site.FindStringSubmatch(target); sm != nil {
			page := "../../site/src/pages/docs/" + sm[1]
			if sm[1] == "" {
				page += "index"
			}
			_, errFile := os.Stat(page + ".md")
			_, errIndex := os.Stat(filepath.Join(page, "index.md"))
			if errFile != nil && errIndex != nil {
				t.Errorf("README.md links to the docs page %q, which the site does not have", target)
			}
			continue
		}
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		path, frag, _ := strings.Cut(target, "#")
		if _, err := os.Stat("../../" + path); err != nil {
			t.Errorf("README.md links to %q, which does not exist", target)
			continue
		}
		if frag != "" && strings.HasSuffix(path, ".md") {
			found := false
			for _, line := range outsideFences(repoFile(t, path)) {
				if h, ok := strings.CutPrefix(strings.TrimLeft(line, "#"), " "); ok && strings.HasPrefix(line, "#") && githubSlug(h) == frag {
					found = true
				}
			}
			if !found {
				t.Errorf("README.md links to %q, but %s has no such heading", target, path)
			}
		}
	}
}

// TestContributingLayoutMatchesTheModules keeps the "Repository layout" of
// CONTRIBUTING.md true to go.work: every module is named, and the drivers
// list is exactly the modules under drivers/.
func TestContributingLayoutMatchesTheModules(t *testing.T) {
	const file = "CONTRIBUTING.md"
	layout := mdSection(t, file, repoFile(t, file), "Repository layout")
	work := repoFile(t, "go.work")
	var drivers []string
	for _, m := range regexp.MustCompile(`(?m)^\s*\./(\S+)$`).FindAllStringSubmatch(work, -1) {
		mod := m[1]
		if name, ok := strings.CutPrefix(mod, "drivers/"); ok {
			drivers = append(drivers, name)
			continue
		}
		if !strings.Contains(layout, "`"+mod+"`") {
			t.Errorf("%s Repository layout does not name the module `%s`", file, mod)
		}
	}
	line := regexp.MustCompile("`drivers/<name>`[^(]*\\(([^)]*)\\)").FindStringSubmatch(layout)
	if line == nil {
		t.Fatalf("%s Repository layout has no \"`drivers/<name>`: ... (`a`, `b`)\" entry", file)
	}
	var listed []string
	for _, m := range regexp.MustCompile("`([a-z0-9]+)`").FindAllStringSubmatch(line[1], -1) {
		listed = append(listed, m[1])
	}
	slices.Sort(listed)
	slices.Sort(drivers)
	if !slices.Equal(listed, drivers) {
		t.Errorf("%s Repository layout lists drivers %q; go.work has %q", file, listed, drivers)
	}
}
