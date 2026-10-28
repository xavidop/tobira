package github

import (
	"errors"
	"regexp"
	"strings"
)

// classifyGraphQL reports whether a GraphQL document is plainly a read: once
// comments and strings are removed, it starts with "{" or the "query"
// keyword and names no mutation or subscription anywhere. Anything else is
// refused, so a document the guard cannot read never runs read-only.
func classifyGraphQL(doc string) error {
	stripped := stripGraphQL(doc)
	trimmed := strings.TrimSpace(stripped)
	switch {
	case trimmed == "":
		return errors.New("the GraphQL document is empty")
	case strings.HasPrefix(trimmed, "{"), queryStartRe.MatchString(trimmed):
	default:
		return errors.New(`only a GraphQL query runs read-only: the document must start with "{" or "query"`)
	}
	if m := writeWordRe.FindStringSubmatch(stripped); m != nil {
		return errors.New("a GraphQL " + m[2] + " is refused read-only")
	}
	return nil
}

var (
	queryStartRe = regexp.MustCompile(`^query(\s|\{|\(|$)`)
	writeWordRe  = regexp.MustCompile(`(^|[^A-Za-z0-9_])(mutation|subscription)([^A-Za-z0-9_]|$)`)
)

// stripGraphQL blanks comments (# to end of line), block strings ("""…""")
// and strings ("…" with escapes), keeping the rest.
func stripGraphQL(doc string) string {
	var b strings.Builder
	for i := 0; i < len(doc); {
		switch {
		case doc[i] == '#':
			for i < len(doc) && doc[i] != '\n' {
				i++
			}
		case strings.HasPrefix(doc[i:], `"""`):
			end := strings.Index(doc[i+3:], `"""`)
			if end < 0 {
				return b.String() + " "
			}
			i += 3 + end + 3
			b.WriteByte(' ')
		case doc[i] == '"':
			i++
			for i < len(doc) && doc[i] != '"' {
				if doc[i] == '\\' {
					i++
				}
				i++
			}
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(doc[i])
			i++
		}
	}
	return b.String()
}
