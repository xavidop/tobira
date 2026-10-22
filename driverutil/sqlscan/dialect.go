package sqlscan

// NumberDot says whether a '.' continues a number token.
type NumberDot uint8

const (
	// DotNever ends a number at a '.': 1.5 is 1, '.', 5.
	DotNever NumberDot = iota
	// DotBeforeDigit continues a number through a '.' followed by a digit.
	DotBeforeDigit
	// DotAlways continues a number through any '.'.
	DotAlways
)

// Dialect describes the lexical rules of one SQL flavour. The zero value
// knows plain whitespace, "--" and non-nesting "/* */" comments, '…' strings
// with doubled-quote escapes, "…" identifiers, ASCII words and digit-only
// numbers; every field adds one rule. The constructors below return the
// dialects the drivers' guards decide on.
type Dialect struct {
	// DashCommentSpace makes "--" open a comment only when it is followed by
	// whitespace, a control character or the end of the text (MySQL).
	DashCommentSpace bool
	// HashComments makes "#" open a line comment (MySQL).
	HashComments bool
	// NestedComments makes "/* */" comments nest (PostgreSQL).
	NestedComments bool
	// ExecComments reports a block comment that the server runs as SQL,
	// "/*! … */" or "/*M! … */", or one holding such an opener in its body,
	// as ExecComment instead of Comment (MySQL).
	ExecComments bool

	// BackslashEscapes makes a backslash escape the next byte in '…'
	// strings, and in "…" strings under DoubleQuoteStrings. It is the
	// session's setting: MySQL without NO_BACKSLASH_ESCAPES, PostgreSQL with
	// standard_conforming_strings off.
	BackslashEscapes bool
	// DoubleQuoteStrings makes "…" a string instead of an identifier (MySQL
	// without ANSI_QUOTES).
	DoubleQuoteStrings bool
	// EStrings makes E'…' (or e'…') a string in which a backslash always
	// escapes the next byte (PostgreSQL).
	EStrings bool
	// DollarQuotes makes $tag$ … $tag$ a string (PostgreSQL). A tag is an
	// ASCII letter or '_' followed by letters, digits and '_', or empty.
	DollarQuotes bool
	// DollarTagHighBit also allows bytes >= 0x80 in a dollar-quote tag.
	DollarTagHighBit bool
	// PrefixedStrings makes N'…', X'…', B'…' (any case) and a charset
	// introducer immediately before a string (_utf8mb4'…') one String token
	// with their prefix.
	PrefixedStrings bool
	// UnicodeEscapes makes U&'…' one String token and U&"…" one
	// QuotedIdent token, with their prefix and without backslash escapes. A
	// following UESCAPE 'c' clause is ordinary Word and String tokens.
	UnicodeEscapes bool

	// IdentBackslashEscapes makes a backslash escape the next byte in "…"
	// and `…` identifiers (ClickHouse).
	IdentBackslashEscapes bool

	// Backticks makes `…` an identifier.
	Backticks bool
	// Brackets makes […] an identifier (SQLite). "]" does not double.
	Brackets bool
	// BracketDoubling makes "]]" inside […] stand for one "]" instead of
	// closing the identifier (T-SQL). It only matters with Brackets.
	BracketDoubling bool

	// IdentDollar lets '$' continue a word.
	IdentDollar bool
	// IdentDollarStart lets '$' start a word (MySQL).
	IdentDollarStart bool
	// IdentHighBit makes bytes >= 0x80 word bytes, so non-ASCII letters
	// belong to words. Otherwise each such byte is Punct.
	IdentHighBit bool

	// NumberIdentChars lets a number run on through word bytes, so 12abc
	// is one Number. Otherwise a number is digits only (12, then abc).
	NumberIdentChars bool
	// NumberDot says whether a '.' continues a number.
	NumberDot NumberDot

	// BOMSpace makes a UTF-8 byte order mark whitespace (SQLite).
	BOMSpace bool
}

// Generic is the dialect for text of no particular database: plain comments,
// '…' strings and "…" identifiers, with Unicode words and numbers such as
// 1.5 and 0xFF.
func Generic() Dialect {
	return Dialect{
		IdentDollar:      true,
		IdentHighBit:     true,
		NumberIdentChars: true,
		NumberDot:        DotBeforeDigit,
	}
}

// SQLite is the dialect of the sqlite driver's read-only guard: "…", `…`
// and […] identifiers, '…' strings, whitespace including a UTF-8 BOM, and
// ASCII words. A '$' or a byte >= 0x80 is Punct, so a statement that uses
// one where the guard expects a keyword is refused.
func SQLite() Dialect {
	return Dialect{
		Backticks:        true,
		Brackets:         true,
		BOMSpace:         true,
		NumberIdentChars: true,
	}
}

// MySQL is the dialect of a MySQL session. noBackslashEscapes and ansiQuotes
// are the NO_BACKSLASH_ESCAPES and ANSI_QUOTES sql_mode flags.
func MySQL(noBackslashEscapes, ansiQuotes bool) Dialect {
	return Dialect{
		DashCommentSpace:   true,
		HashComments:       true,
		ExecComments:       true,
		BackslashEscapes:   !noBackslashEscapes,
		DoubleQuoteStrings: !ansiQuotes,
		Backticks:          true,
		IdentDollar:        true,
		IdentDollarStart:   true,
		IdentHighBit:       true,
		NumberIdentChars:   true,
		NumberDot:          DotAlways,
	}
}

// Postgres is the dialect of a PostgreSQL session. stdStrings is
// standard_conforming_strings: when it is off a backslash escapes the next
// byte in '…'. Dollar-quote tags may hold bytes >= 0x80 ($é$), as in the
// server's scanner. Numbers are digits only, so 1into is a number and the
// word INTO, as older servers read it.
func Postgres(stdStrings bool) Dialect {
	return Dialect{
		NestedComments:   true,
		BackslashEscapes: !stdStrings,
		EStrings:         true,
		DollarQuotes:     true,
		DollarTagHighBit: true,
		IdentDollar:      true,
		IdentHighBit:     true,
	}
}

// ClickHouse is the dialect of a ClickHouse query: "--", "#" and nesting
// "/* */" comments; '…' strings and "…" and `…` identifiers, all with
// backslash escapes and doubled-quote escapes; $tag$ … $tag$ heredoc
// strings; words with '$' and bytes >= 0x80; numbers such as 1_000, 0x1F
// and 1.5e3.
//
// ClickHouse itself reads "#" as a comment only before a space or '!'; any
// other "#" is a syntax error there, so reading it as a comment never
// accepts text the server would run differently.
func ClickHouse() Dialect {
	return Dialect{
		HashComments:          true,
		NestedComments:        true,
		BackslashEscapes:      true,
		IdentBackslashEscapes: true,
		DollarQuotes:          true,
		Backticks:             true,
		IdentDollar:           true,
		IdentHighBit:          true,
		NumberIdentChars:      true,
		NumberDot:             DotBeforeDigit,
	}
}
