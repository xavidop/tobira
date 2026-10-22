package sqlscan

// TSQL is the dialect of the mssql driver's read-only guard (Microsoft SQL
// Server): "--" and nesting "/* */" comments; '…' strings with doubled
// quotes and no backslash escapes; […] identifiers in which "]]" stands for
// "]", and "…" identifiers (strings under QUOTED_IDENTIFIER OFF, with the
// same boundaries). N'…' scans as the Word N and then the String.
//
// Words are ASCII letters, digits, '_' and '$'. A byte >= 0x80, '@' and '#'
// are Punct, so a variable (@p1) or temporary table (#t) is Punct then a
// Word, and a word the server would read differently is split, never
// merged. A number runs on through word bytes and dots (0x1F, 1e5,
// 1.eINSERT, 1INTO are one Number each): the guard checks a number's shape
// itself, because the server reads 0x and an exponent as part of a number
// and the rest as a separate word.
func TSQL() Dialect {
	return Dialect{
		NestedComments:   true,
		Brackets:         true,
		BracketDoubling:  true,
		IdentDollar:      true,
		NumberIdentChars: true,
		NumberDot:        DotAlways,
	}
}

// bracketDoubled scans the […] identifier opened at s[i], in which "]]"
// stands for "]".
func bracketDoubled(s string, i int) (Kind, int) {
	for j := i + 1; j < len(s); j++ {
		if s[j] != ']' {
			continue
		}
		if j+1 < len(s) && s[j+1] == ']' {
			j++
			continue
		}
		return QuotedIdent, j + 1
	}
	return Unterminated, len(s)
}
