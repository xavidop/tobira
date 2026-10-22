package editor

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/xavidop/tobira/internal/tui/theme"
)

// lexMargin is how many lines above the view are lexed with it. Lines below
// the view cannot change its colours, so they are never lexed. A buffer whose
// view starts within lexMargin lines of the top is lexed from its first line
// and always coloured right; further down, a string or comment opened more
// than lexMargin lines above the view may be coloured wrongly. In exchange a
// keystroke re-lexes at most lexMargin plus a screenful of lines, however big
// the buffer is (chroma costs tens of microseconds a line).
const lexMargin = 100

// run colours bytes [start, end) of a line with style st (an index into
// highlighter.styles).
type run struct {
	start, end int
	st         int
	tt         chroma.TokenType
}

// highlighter turns buffer lines into coloured runs. It caches the runs of
// one buffer version and line window, so a frame never lexes again.
type highlighter struct {
	lexer  chroma.Lexer // nil: no highlighting
	style  *chroma.Style
	styles []lipgloss.Style // 0 is plain text
	byType map[chroma.TokenType]int

	valid   bool
	version uint64
	lo, hi  int     // the lines lexed: [lo, hi)
	runs    [][]run // runs[i-lo] for line i
}

// lexerFor maps a driver dialect (driver.Info.Dialect) to a chroma lexer:
// postgres, mysql, T-SQL and CQL have their own, DuckDB reads as PostgreSQL,
// the drivers whose queries are JSON bodies (and "json", the cell editor's)
// use the JSON lexer, everything else falls back to plain SQL, and to no
// highlighting if even that is missing.
func lexerFor(dialect string) chroma.Lexer {
	name := "sql"
	switch strings.ToLower(dialect) {
	case "postgresql", "postgres", "duckdb":
		name = "postgresql"
	case "mysql", "mariadb":
		name = "mysql"
	case "tsql":
		name = "tsql"
	case "cql":
		name = "cql"
	case "json", // the inline cell editor's JSON values
		"elasticsearch", "opensearch", "firestore-json", "pinecone", "weaviate", "chroma", "milvus",
		"mongodb", "qdrant":
		name = "json"
	}
	if l := lexers.GlobalLexerRegistry.Get(name); l != nil {
		return l
	}
	return lexers.GlobalLexerRegistry.Get("sql")
}

func newHighlighter(th theme.Theme, dialect string) *highlighter {
	h := &highlighter{lexer: lexerFor(dialect), style: chromaStyle(th), byType: map[chroma.TokenType]int{}}
	h.styles = []lipgloss.Style{lipgloss.NewStyle().Foreground(th.Fg)}
	return h
}

// hex renders c as #rrggbb for a chroma style entry.
func hex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// chromaStyle builds a chroma style from the tobira palette, so the editor
// follows the theme (dark or light) instead of a stock chroma style.
func chromaStyle(th theme.Theme) *chroma.Style {
	accent := hex(th.Styles.Title.GetForeground()) // the accent as readable text
	fg, muted := hex(th.Fg), hex(th.Muted)
	return chroma.MustNewStyle("tobira-"+th.Name, chroma.StyleEntries{
		chroma.Text:            fg,
		chroma.Keyword:         "bold " + accent,
		chroma.Name:            fg,
		chroma.NameBuiltin:     accent,
		chroma.NameFunction:    accent,
		chroma.LiteralString:   hex(th.OK),
		chroma.LiteralNumber:   hex(th.Warn),
		chroma.Comment:         "italic " + muted,
		chroma.CommentPreproc:  muted,
		chroma.Operator:        fg,
		chroma.OperatorWord:    "bold " + accent,
		chroma.Punctuation:     muted,
		chroma.Error:           hex(th.Danger),
		chroma.GenericDeleted:  hex(th.Danger),
		chroma.GenericInserted: hex(th.OK),
	})
}

// styleOf returns the style index of a token type, building it on first use.
func (h *highlighter) styleOf(tt chroma.TokenType) int {
	if i, ok := h.byType[tt]; ok {
		return i
	}
	e := h.style.Get(tt)
	st := lipgloss.NewStyle()
	if e.Colour.IsSet() {
		st = st.Foreground(lipgloss.Color(e.Colour.String()))
	}
	if e.Bold == chroma.Yes {
		st = st.Bold(true)
	}
	if e.Italic == chroma.Yes {
		st = st.Italic(true)
	}
	h.styles = append(h.styles, st)
	h.byType[tt] = len(h.styles) - 1
	return len(h.styles) - 1
}

// window is the range of lines to lex so that [top, bottom) is coloured.
func window(n, top, bottom int) (lo, hi int) {
	return max(0, top-lexMargin), min(n, bottom)
}

// lineRuns returns the runs of line i, lexing again when the buffer changed
// or the view moved outside the cached window. Nil means plain text.
func (h *highlighter) lineRuns(m *Model, top, bottom, i int) []run {
	if h.lexer == nil {
		return nil
	}
	if !h.valid || h.version != m.version || top < h.lo || bottom > h.hi {
		h.lex(m, top, bottom)
	}
	if i < h.lo || i >= h.hi {
		return nil
	}
	return h.runs[i-h.lo]
}

func (h *highlighter) lex(m *Model, top, bottom int) {
	lo, hi := window(len(m.lines), top, bottom)
	h.valid, h.version, h.lo, h.hi = true, m.version, lo, hi
	h.runs = make([][]run, hi-lo)
	it, err := h.lexer.Tokenise(nil, strings.Join(m.lines[lo:hi], "\n"))
	if err != nil {
		return // plain text
	}
	row, col := 0, 0
	for tok := it(); tok != chroma.EOF; tok = it() {
		st := h.styleOf(tok.Type)
		for v := tok.Value; v != "" && row < len(h.runs); {
			piece, rest, nl := strings.Cut(v, "\n")
			if piece != "" {
				line := m.lines[lo+row]
				end := min(col+len(piece), len(line))
				if col < end {
					h.runs[row] = append(h.runs[row], run{col, end, st, tok.Type})
				}
				col = end
			}
			if !nl {
				break
			}
			row, col, v = row+1, 0, rest
		}
	}
}

// literal reports whether token type tt, in dialect, is inside a string
// literal or a comment. Quoted names (double quotes outside mysql,
// backticks) are strings to chroma but names to SQL, so they do not count.
func literal(tt chroma.TokenType, dialect string) bool {
	if tt.InCategory(chroma.Comment) {
		return true
	}
	if !tt.InSubCategory(chroma.LiteralString) {
		return false
	}
	switch tt {
	case chroma.LiteralStringAffix, chroma.LiteralStringName, chroma.LiteralStringBacktick:
		return false
	case chroma.LiteralStringDouble:
		d := strings.ToLower(dialect)
		return d == "mysql" || d == "mariadb"
	}
	return true
}

// CursorInLiteral reports whether the character before the cursor is in a
// string literal or a comment, as the last highlighting pass lexed it. It
// never lexes: known is false when that pass was of another buffer version
// or did not cover the cursor's line (or there is no highlighting).
func (m Model) CursorInLiteral() (in, known bool) {
	h := m.hl
	if h == nil || h.lexer == nil || !h.valid || h.version != m.version || m.cur.row < h.lo || m.cur.row >= h.hi {
		return false, false
	}
	if m.cur.col == 0 {
		return false, true
	}
	b := m.cur.col - 1
	for _, r := range h.runs[m.cur.row-h.lo] {
		if r.start <= b && b < r.end {
			return literal(r.tt, m.dialect), true
		}
	}
	return false, true
}
