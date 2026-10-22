package ai

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/firebase/genkit/go/genkit"
	"github.com/goccy/go-yaml"
	"github.com/google/dotprompt/go/dotprompt"

	gai "github.com/firebase/genkit/go/ai"
)

// The prompt names: ai/prompts/<name>.prompt, overridable by
// <Deps.PromptDir>/<name>.prompt.
const (
	promptAsk            = "ask"
	promptAnswer         = "answer"
	promptExplainResults = "explain_results"
	promptFixError       = "fix_error"
)

func promptNames() []string {
	return []string{promptAsk, promptAnswer, promptExplainResults, promptFixError}
}

// promptFS holds the built-in Dotprompt files.
//
//go:embed prompts/*.prompt
var promptFS embed.FS

// noPromptFS is handed to genkit.Init so that it loads no prompt files by
// itself (without a prompt filesystem it loads ./prompts from the working
// directory). The prompts are registered explicitly, after the overrides
// are applied: see registerPrompts.
var noPromptFS embed.FS

// promptSource is the text of one prompt and where it came from, for
// error messages.
type promptSource struct {
	name, file, text string
}

// loadPromptSources returns the text of every prompt: the built-in one, or
// the file of the same name in dir when there is one. An override that
// is not a regular file, cannot be read or does not render is an error
// naming the file. Other files in dir are ignored, with a debug log. dir
// "" or missing means no overrides.
func loadPromptSources(dir string, log *slog.Logger) (map[string]promptSource, error) {
	srcs := map[string]promptSource{}
	for _, name := range promptNames() {
		file := "prompts/" + name + ".prompt"
		b, err := promptFS.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("built-in prompt %s: %w", file, err)
		}
		srcs[name] = promptSource{name: name, file: "built-in " + file, text: string(b)}
	}
	if dir == "" {
		return srcs, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return srcs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prompt overrides %s: %w", dir, err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".prompt")
		if _, known := srcs[name]; !ok || !known || e.IsDir() {
			log.Debug("ignoring file in the prompt override directory", "file", e.Name())
			continue
		}
		path := filepath.Join(dir, e.Name())
		// Stat follows a symlink; anything but a regular file (a FIFO would
		// block the read forever) is refused.
		if fi, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("prompt override %s: %w", path, err)
		} else if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid prompt override %s: not a regular file", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("prompt override %s: %w", path, err)
		}
		if err := validatePrompt(name, string(b)); err != nil {
			return nil, fmt.Errorf("invalid prompt override %s: %w", path, err)
		}
		srcs[name] = promptSource{name: name, file: path, text: string(b)}
		log.Debug("prompt override", "prompt", name, "file", path)
	}
	return srcs, nil
}

// validatePrompt parses text as Dotprompt and renders it with a sample
// input, which catches frontmatter, schema and template errors. The
// frontmatter is parsed first on its own: Dotprompt treats unparsable
// frontmatter as part of the template instead of failing.
func validatePrompt(name, text string) (err error) {
	defer func() {
		if r := recover(); r != nil { // the YAML parser can panic on malformed input
			err = fmt.Errorf("%v", r)
		}
	}()
	if m := dotprompt.FrontmatterAndBodyRegex.FindStringSubmatch(text); m != nil && strings.TrimSpace(m[1]) != "" {
		var meta map[string]any
		if err := yaml.Unmarshal([]byte(m[1]), &meta); err != nil {
			return fmt.Errorf("frontmatter: %w", err)
		}
	}
	_, err = dotprompt.NewDotprompt(nil).Render(text, &dotprompt.DataArgument{Input: sampleInput(name)}, nil)
	return err
}

// sampleInput is an input with every field a prompt can use, for
// validation.
func sampleInput(name string) map[string]any {
	in := map[string]any{"dialect": "sqlite", "kind": "sql", "schema": "dialect: sqlite", "nonce": "0123456789abcdef"}
	switch name {
	case promptAsk, promptAnswer:
		in["question"] = "how many rows?"
		in["history"] = []map[string]any{{"from": "user", "text": "hi"}}
		in["tools"], in["execute"] = true, true
	case promptExplainResults:
		in["query"], in["columns"], in["rows"], in["note"], in["truncated"] = "select 1", "[]", "[]", "", false
	case promptFixError:
		in["query"], in["error"], in["tools"] = "select 1", "syntax error", true
	}
	return in
}

// registerPrompts defines every prompt on g under its own name.
func registerPrompts(g *genkit.Genkit, srcs map[string]promptSource) (map[string]gai.Prompt, error) {
	out := map[string]gai.Prompt{}
	for _, name := range promptNames() {
		src := srcs[name]
		p, err := genkit.LoadPromptFromSource(g, src.text, name, "")
		if err != nil {
			return nil, fmt.Errorf("prompt %s: %w", src.file, err)
		}
		out[name] = p
	}
	return out, nil
}

// Data fences. Dotprompt renders variables unescaped, so a prompt cannot
// tell data from its own text by content. Instead every render draws a
// random nonce (16 hex characters from crypto/rand) and the prompts wrap
// each data section (schema context, conversation, query, error, columns,
// rows) in <data-NONCE kind="..."> ... </data-NONCE>; their system rules say
// that only text inside those fences is data and that it is never followed.
// Data cannot close its fence: it would have to guess the nonce. The
// question itself is the user's request and stays outside the fences.
//
// Data is passed through byte for byte, with two exceptions:
//   - "<<<dotprompt:" (case-sensitive, what Dotprompt splits messages on:
//     role, history, media and section markers) becomes "<<< dotprompt:",
//     so data cannot open a message of its own;
//   - "data-NONCE" in an input is near impossible (a fresh nonce is drawn
//     when an input contains it), but should every draw collide it becomes
//     "data -NONCE".

// nonceDraws is how many nonces fence draws before it breaks a collision
// in the data instead.
const nonceDraws = 8

// newNonce returns 16 random hex characters.
func newNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err)) // never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// fence prepares a prompt input: it draws a nonce (with next) that no
// string in input contains, breaks the Dotprompt markers in every string,
// and sets the "nonce" variable.
func fence(input map[string]any, next func() string) map[string]any {
	var nonce string
	for range nonceDraws {
		nonce = next()
		if !containsString(input, "data-"+nonce) {
			break
		}
	}
	out := mapStrings(input, func(s string) string {
		s = strings.ReplaceAll(s, "<<<dotprompt:", "<<< dotprompt:")
		return strings.ReplaceAll(s, "data-"+nonce, "data -"+nonce)
	}).(map[string]any)
	out["nonce"] = nonce
	return out
}

// containsString reports whether any string in v contains sub.
func containsString(v any, sub string) bool {
	found := false
	mapStrings(v, func(s string) string {
		found = found || strings.Contains(s, sub)
		return s
	})
	return found
}

// mapStrings returns v with fn applied to every string in it: the values of
// a map, and the strings in nested maps and slices.
func mapStrings(v any, fn func(string) string) any {
	switch x := v.(type) {
	case string:
		return fn(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = mapStrings(e, fn)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, e := range x {
			out[i] = mapStrings(e, fn).(map[string]any)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = mapStrings(e, fn)
		}
		return out
	default:
		return v
	}
}
