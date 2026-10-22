package postgres

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Server-side file access on the read-only path.
//
// BEGIN READ ONLY stops writes to the database, but not every way out of it:
// COPY … TO '/file' or TO PROGRAM, and functions such as lo_export, write on
// the server's filesystem from inside a read-only transaction. So on the
// read-only path (connection read_only or a per-call ReadOnly) Query refuses,
// before anything runs:
//
//   - a statement whose first keyword is COPY, or DO (an anonymous block can
//     build SQL at run time, which no lexical check sees);
//   - text naming one of serverFileFunctions as an identifier (outside
//     strings and comments, case-insensitive, quoted identifiers included);
//   - a change to a setting that alters how '…' is lexed (changesStringGUC).
//
// Every text scan is done with '…' as a standard string and with backslash
// escapes in it (standard_conforming_strings off, which a role or database
// default can set), each with PostgreSQL's dollar-quote tags (bytes >= 0x80
// allowed) and with ASCII-only ones (see guardDialects), and refuses when any
// lexing does. First
// keywords (COPY, DO, SET) come before any string, so they need one scan.
//
// This is defence in depth on top of BEGIN READ ONLY, like the mysql guard
// that refuses INTO OUTFILE. It is lexical, so SQL a function body builds at
// run time is not seen: read-only mode guards against accidental writes, it is
// not a sandbox against a superuser. A role without superuser,
// pg_write_server_files or pg_execute_server_program is the boundary. The
// Exporter builds its own COPY after its own checks, and refuses the same
// things in the query it wraps (serverAccessRefusal) on every connection,
// because its BEGIN READ ONLY does not stop file access either.

// serverFileFunctions are the functions that read or write server files (or
// create and drop large objects from them), lower-cased.
var serverFileFunctions = []string{
	"lo_export", "lo_import", "lo_unlink",
	"pg_file_write", "pg_file_rename", "pg_file_unlink", "pg_logdir_ls",
}

// serverAccessRefusal returns the ErrReadOnly refusal for text on a
// read-only path (op names the call), or nil: COPY, DO, a change to a
// setting of stringGUCs, or a server-file function. Query applies it on the
// read-only path; Export, which always runs in BEGIN READ ONLY, applies it
// to every export.
func serverAccessRefusal(op, text string) error {
	refuse := func(err error) error { return driver.Wrap(driver.ErrReadOnly, Name, op, err) }
	switch {
	case isCopy(text):
		// COPY … TO a file or PROGRAM writes on the server inside READ ONLY.
		return refuse(errors.New("COPY is not allowed on a read-only path"))
	case isDO(text):
		// An anonymous block can build SQL at run time that no lexical check sees.
		return refuse(errors.New("DO blocks are not allowed on a read-only path"))
	}
	if g, ok := changesStringGUC(text); ok {
		// The guards lex '…' every way, but the setting stays fixed anyway.
		return refuse(fmt.Errorf("changing %s is not allowed on a read-only path", g))
	}
	if f, ok := serverFileFunction(text); ok {
		return refuse(fmt.Errorf("%s (server-side file access) is not allowed on a read-only path", f))
	}
	return nil
}

// isDO reports whether the first keyword of text is DO. An anonymous code
// block can build SQL at run time (EXECUTE 'COPY …'), which no lexical check
// sees, so DO is refused on the read-only path.
func isDO(text string) bool { return leadingKeyword(text) == "DO" }

// isCopy reports whether the first keyword of text is COPY.
func isCopy(text string) bool { return leadingKeyword(text) == "COPY" }

// serverFileFunction returns the first of serverFileFunctions that text names
// as an identifier, and whether there is one. Schema qualification does not
// hide it: pg_catalog.lo_export scans as two words.
func serverFileFunction(text string) (string, bool) {
	for _, words := range lexings(text, true) { // any lexing seeing one is enough
		for _, w := range words {
			for _, f := range serverFileFunctions {
				if strings.EqualFold(w.text, f) {
					return f, true
				}
			}
		}
	}
	return "", false
}

// stringGUCs are the settings that change how '…' is lexed, lower-cased.
var stringGUCs = []string{"standard_conforming_strings", "backslash_quote", "escape_string_warning"}

// changesStringGUC returns the setting of stringGUCs that text changes, and
// whether it does: SET [LOCAL|SESSION] or RESET naming it as an identifier,
// or a set_config call anywhere in text that mentions it (its name is a
// string there, so any mention counts). Any lexing of the text counts.
func changesStringGUC(text string) (string, bool) {
	kw := leadingKeyword(text)
	lower := strings.ToLower(text)
	for _, words := range lexings(text, true) {
		for _, w := range words {
			if w.text == "SET_CONFIG" {
				for _, g := range stringGUCs {
					if strings.Contains(lower, g) {
						return g, true
					}
				}
			}
			if kw != "SET" && kw != "RESET" {
				continue
			}
			for _, g := range stringGUCs {
				if strings.EqualFold(w.text, g) {
					return g, true
				}
			}
		}
	}
	return "", false
}

// quotedIdent returns the name inside a "…" identifier (q includes the
// quotes; an unterminated one runs to the end), with "" undoubled.
func quotedIdent(q string) string {
	return strings.ReplaceAll(strings.TrimSuffix(q[1:], `"`), `""`, `"`)
}

// uescape reads an optional UESCAPE 'c' clause at text[i:]. It returns the
// escape character (backslash by default) and the index after the clause, or
// i when there is none.
func uescape(text string, i int) (byte, int) {
	j := skipBlank(text, i)
	if !strings.EqualFold(leadingKeyword(text[j:]), "UESCAPE") {
		return '\\', i
	}
	k := skipBlank(text, j+len("UESCAPE"))
	if k+2 < len(text) && text[k] == '\'' && text[k+2] == '\'' {
		return text[k+1], k + 3
	}
	return '\\', i
}

// unicodeIdent decodes the escapes of a U&"…" identifier: esc esc is esc,
// esc XXXX and esc +XXXXXX are code points. A malformed escape is kept as is.
func unicodeIdent(s string, esc byte) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != esc {
			b.WriteByte(s[i])
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == esc:
			b.WriteByte(esc)
			i++
		case hexRune(s, i+1, 4, &b):
			i += 4
		case i+1 < len(s) && s[i+1] == '+' && hexRune(s, i+2, 6, &b):
			i += 7
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// hexRune writes the code point spelled by the n hex digits at s[i:] and
// reports whether there were n hex digits.
func hexRune(s string, i, n int, b *strings.Builder) bool {
	if i+n > len(s) {
		return false
	}
	v, err := strconv.ParseUint(s[i:i+n], 16, 32)
	if err != nil {
		return false
	}
	b.WriteRune(rune(v))
	return true
}
