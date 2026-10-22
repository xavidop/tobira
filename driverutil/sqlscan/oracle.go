package sqlscan

// ScanOracle splits s into tokens the way Oracle Database reads SQL text and
// calls fn with each one in order, stopping early when fn returns false. It
// is a scanner of its own rather than a Dialect because two of Oracle's
// rules have no Dialect field:
//
//   - q'…' and nq'…' (any case) are alternative-quoting strings. The
//     character after the quote is the delimiter; [ { ( < close with
//     ] } ) >, any other character closes with itself, and the string ends
//     at the closing delimiter followed by a quote. A space, tab, newline,
//     quote or non-ASCII byte as the delimiter (Oracle accepts multibyte
//     delimiters; this scanner does not match them), or a missing end,
//     yields Unterminated.
//   - '$' and '#' continue a word (v$session, a#b) but do not start one.
//
// The other rules: whitespace is space, tab, newline, CR, form feed and
// vertical tab; "--" opens a comment that runs to the next newline (a lone
// CR does not end it: a guard should refuse a line comment holding one,
// since the server's handling of it was not verified); "/* */" comments do
// not nest (a hint, /*+ … */, is a Comment); '…' strings double the quote
// and have no backslash escapes; "…" identifiers end at the next '"' (Oracle
// has no escape for '"' inside one); words start with an ASCII letter, '_'
// or a byte >= 0x80; numbers are digits with an optional fraction, an
// exponent with an optional sign and an f, F, d or D suffix, and run on
// through word bytes so that 1delete is one Number (the server reads it as
// 1d followed by the word elete), which a guard can refuse as malformed.
// N'…' scans as a Word then a String. Any other byte, NUL included, is Punct.
func ScanOracle(s string, fn func(Token) bool) {
	for i := 0; i < len(s); {
		kind, end := oracleNext(s, i)
		if !fn(Token{Kind: kind, Start: i, End: end}) {
			return
		}
		i = end
	}
}

// oracleNext returns the kind and end of the token starting at s[i].
func oracleNext(s string, i int) (Kind, int) {
	n := len(s)
	c := s[i]
	switch {
	case isSpace(c):
		j := i + 1
		for j < n && isSpace(s[j]) {
			j++
		}
		return Space, j
	case c == '-' && i+1 < n && s[i+1] == '-':
		for j := i; j < n; j++ {
			if s[j] == '\n' {
				return Comment, j
			}
		}
		return Comment, n
	case c == '/' && i+1 < n && s[i+1] == '*':
		return Dialect{}.blockComment(s, i)
	case c == '\'':
		return quoted(s, i, '\'', false, String)
	case c == '"':
		for j := i + 1; j < n; j++ {
			if s[j] == '"' {
				return QuotedIdent, j + 1
			}
		}
		return Unterminated, n
	case c == ';':
		return Semicolon, i + 1
	case isDigit(c):
		return Number, oracleNumberEnd(s, i)
	case isASCIILetter(c) || c == '_' || c >= 0x80:
		j := i + 1
		for j < n && oracleWordByte(s[j]) {
			j++
		}
		if j < n && s[j] == '\'' && isQPrefix(s[i:j]) {
			return qQuote(s, j)
		}
		return Word, j
	}
	return Punct, i + 1
}

func oracleWordByte(c byte) bool {
	return isASCIILetter(c) || isDigit(c) || c == '_' || c == '$' || c == '#' || c >= 0x80
}

// isQPrefix reports whether w prefixes an alternative-quoting string.
func isQPrefix(w string) bool {
	switch w {
	case "q", "Q", "nq", "nQ", "Nq", "NQ":
		return true
	}
	return false
}

// qQuote scans the alternative-quoting string whose quote is at s[q].
func qQuote(s string, q int) (Kind, int) {
	if q+1 >= len(s) {
		return Unterminated, len(s)
	}
	open := s[q+1]
	var closer byte
	if open >= 0x80 { // a multibyte delimiter: refused rather than matched by rune
		return Unterminated, len(s)
	}
	switch open {
	case ' ', '\t', '\n', '\r', '\'':
		return Unterminated, len(s)
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '(':
		closer = ')'
	case '<':
		closer = '>'
	default:
		closer = open
	}
	for j := q + 2; j+1 < len(s); j++ {
		if s[j] == closer && s[j+1] == '\'' {
			return String, j + 2
		}
	}
	return Unterminated, len(s)
}

// oracleNumberEnd returns the end of the number at s[i]: digits, an optional
// fraction, an optional exponent and suffix, then any word bytes.
func oracleNumberEnd(s string, i int) int {
	n := len(s)
	j := i
	for j < n && isDigit(s[j]) {
		j++
	}
	if j < n && s[j] == '.' {
		j++
		for j < n && isDigit(s[j]) {
			j++
		}
	}
	if j < n && (s[j] == 'e' || s[j] == 'E') {
		k := j + 1
		if k < n && (s[k] == '+' || s[k] == '-') {
			k++
		}
		if k < n && isDigit(s[k]) {
			j = k
			for j < n && isDigit(s[j]) {
				j++
			}
		}
	}
	for j < n && oracleWordByte(s[j]) {
		j++
	}
	return j
}
