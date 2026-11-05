// Package federate runs one query over several connections: each source is
// staged with its own native query, read-only, into a scratch SQLite
// database, and a final SQLite statement joins them there. See
// docs/design.md for the shape and the limits.
package federate

import (
	"fmt"
	"strings"
)

// Source is one @source directive: the alias table the staged rows fill and
// the native query that fills it.
type Source struct {
	Alias     string
	Conn      string
	Namespace string // "" = the connection's default
	Query     string
	Line      int // 1-based line of the directive
}

// Sink is the @sink directive: the collection the final result is written
// to. A statement has at most one.
type Sink struct {
	Conn       string
	Namespace  string
	Collection string
	Line       int
}

// Statement is a parsed federated statement.
type Statement struct {
	Sources []Source
	Sink    *Sink
	// Final is the SQLite statement run over the alias tables, with the
	// directive lines removed and the other lines kept in place.
	Final string
}

// ParseError is a directive that could not be read, with its line.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// The directive keywords, as they follow "-- @".
const (
	kwSource = "source"
	kwSink   = "sink"
)

// IsFederated reports whether text holds a @source directive, which is what
// makes a buffer a federated statement. A buffer without one runs as today.
func IsFederated(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if kw, _, ok := directive(line); ok && kw == kwSource {
			return true
		}
	}
	return false
}

// Parse reads the directives of text and the final statement around them.
func Parse(text string) (*Statement, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	st := &Statement{}
	aliases := map[string]int{}
	var final []string
	for i := 0; i < len(lines); i++ {
		n := i + 1
		kw, rest, ok := directive(lines[i])
		if !ok {
			final = append(final, lines[i])
			continue
		}
		switch kw {
		case kwSource:
			src, err := parseSource(rest, n)
			if err != nil {
				return nil, err
			}
			// The query continues on the comment lines that follow, so a
			// long native query stays a comment to every highlighter.
			var more []string
			for i+1 < len(lines) {
				body, isComment := commentBody(lines[i+1])
				if !isComment {
					break
				}
				if _, _, isDirective := directive(lines[i+1]); isDirective {
					break
				}
				more = append(more, body)
				i++
			}
			if len(more) > 0 {
				src.Query = strings.TrimSpace(src.Query + "\n" + strings.Join(more, "\n"))
			}
			if src.Query == "" {
				return nil, &ParseError{n, "source " + src.Alias + " has no query"}
			}
			if prev, dup := aliases[src.Alias]; dup {
				return nil, &ParseError{n, fmt.Sprintf("alias %s is already defined on line %d", src.Alias, prev)}
			}
			aliases[src.Alias] = n
			st.Sources = append(st.Sources, src)
		case kwSink:
			if st.Sink != nil {
				return nil, &ParseError{n, fmt.Sprintf("a statement has one sink; the first is on line %d", st.Sink.Line)}
			}
			sink, err := parseSink(rest, n)
			if err != nil {
				return nil, err
			}
			st.Sink = sink
		default:
			return nil, &ParseError{n, "unknown directive @" + kw + " (use @source or @sink)"}
		}
	}
	if len(st.Sources) == 0 {
		return nil, &ParseError{1, "no @source directive"}
	}
	st.Final = strings.TrimSpace(strings.Join(final, "\n"))
	if st.Final == "" {
		return nil, &ParseError{len(lines), "no final statement after the directives"}
	}
	return st, nil
}

// directive reads a "-- @<keyword> <rest>" line. ok is false for any other
// line, including an ordinary comment.
func directive(line string) (keyword, rest string, ok bool) {
	body, isComment := commentBody(line)
	if !isComment {
		return "", "", false
	}
	body = strings.TrimLeft(body, " \t")
	if !strings.HasPrefix(body, "@") {
		return "", "", false
	}
	body = body[1:]
	end := strings.IndexAny(body, " \t")
	if end < 0 {
		return body, "", body != ""
	}
	keyword, rest = body[:end], strings.TrimSpace(body[end:])
	return keyword, rest, keyword != ""
}

// commentBody returns what follows the "--" of a line comment, with one
// leading space dropped, and whether the line is a line comment at all.
func commentBody(line string) (string, bool) {
	t := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(t, "--") {
		return "", false
	}
	t = strings.TrimPrefix(t[2:], " ")
	return strings.TrimRight(t, " \t\r"), true
}

// parseSource reads "<alias> = <connection>[/<namespace>]: <query>".
func parseSource(rest string, line int) (Source, error) {
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return Source{}, &ParseError{line, "expected @source <alias> = <connection>: <query>"}
	}
	alias := strings.TrimSpace(rest[:eq])
	if !isIdent(alias) {
		return Source{}, &ParseError{line, fmt.Sprintf("alias %q must be an identifier (letters, digits and _, not starting with a digit)", alias)}
	}
	target, query, err := parseTarget(strings.TrimSpace(rest[eq+1:]), line)
	if err != nil {
		return Source{}, err
	}
	return Source{Alias: alias, Conn: target.conn, Namespace: target.ns, Query: strings.TrimSpace(query), Line: line}, nil
}

// parseSink reads "<connection>[/<namespace>]: <collection>".
func parseSink(rest string, line int) (*Sink, error) {
	target, coll, err := parseTarget(rest, line)
	if err != nil {
		return nil, err
	}
	coll = strings.TrimSpace(coll)
	if coll == "" {
		return nil, &ParseError{line, "sink has no collection"}
	}
	return &Sink{Conn: target.conn, Namespace: target.ns, Collection: coll, Line: line}, nil
}

// target is a connection and an optional namespace.
type target struct{ conn, ns string }

// parseTarget reads "<connection>[/<namespace>]: <tail>" and returns the
// target and the tail. A connection or namespace with a space, "/" or ":"
// in it is written in double quotes.
func parseTarget(s string, line int) (target, string, error) {
	conn, i, err := name(s, 0, line, "connection")
	if err != nil {
		return target{}, "", err
	}
	var ns string
	if i < len(s) && s[i] == '/' {
		if ns, i, err = name(s, i+1, line, "namespace"); err != nil {
			return target{}, "", err
		}
	}
	if i >= len(s) || s[i] != ':' {
		return target{}, "", &ParseError{line, "expected \":\" after the connection"}
	}
	return target{conn: conn, ns: ns}, s[i+1:], nil
}

// name reads a connection or namespace name at s[i:]: a quoted one up to
// its closing quote, else up to the first "/" or ":". It returns the name
// and the index after it.
func name(s string, i, line int, what string) (string, int, error) {
	if i < len(s) && s[i] == '"' {
		end := strings.IndexByte(s[i+1:], '"')
		if end < 0 {
			return "", 0, &ParseError{line, what + " name is missing its closing quote"}
		}
		n := s[i+1 : i+1+end]
		if n == "" {
			return "", 0, &ParseError{line, what + " name is empty"}
		}
		return n, i + 2 + end, nil
	}
	end := strings.IndexAny(s[i:], "/:")
	if end < 0 {
		end = len(s) - i
	}
	n := strings.TrimSpace(s[i : i+end])
	if n == "" {
		return "", 0, &ParseError{line, what + " name is empty"}
	}
	return n, i + end, nil
}

// isIdent reports whether s is [A-Za-z_][A-Za-z0-9_]*.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
