package driver

import (
	"net/url"
	"slices"
	"strings"
)

const maskedPassword = "xxxxx"

// minBarePassword is the shortest password RedactMessage masks wherever it
// appears in a message. A shorter one is masked only in structured positions
// (userinfo, key=value, "key": value), so a 1-character password cannot
// scramble the whole message.
const minBarePassword = 4

// Scheme returns the lower-cased scheme of a DSN: "postgres" for
// "postgres://…", "sqlite" for "sqlite:./x.db" and "file" for "file:x.db".
// It returns "" when dsn has no scheme, as for a path or a key=value DSN.
func Scheme(dsn string) string {
	i := strings.IndexByte(dsn, ':')
	// A single-letter "scheme" is a Windows drive such as C:\data.
	if i < 2 {
		return ""
	}
	for j := 0; j < i; j++ {
		c := dsn[j]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if j == 0 && !letter {
			return ""
		}
		if !letter && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return ""
		}
	}
	return strings.ToLower(dsn[:i])
}

// RedactDSN replaces the password in a DSN with "xxxxx". It does not guess
// the DSN's form: it runs every reading below on the whole text and masks
// the union of the password spans they find, so a DSN that one reading
// misparses is still covered by another. Over-redaction is preferred to
// leaking. The readings are:
//
//   - URL userinfo: scheme://user:password@host/db. Everything between the
//     first ':' of the authority and the last '@' outside a password
//     parameter is masked, so passwords with unescaped '#', '?', '/' or '@'
//     do not leak.
//   - userinfo without "://": the go-sql-driver user:password@tcp(host)/db
//     and user:password@/db forms (and any opaque DSN shaped like them):
//     between the first ':' and the last '@', when no '=' or space comes
//     before that ':'. An odbc: string has no userinfo.
//   - query and ';' parameters: [?&;]key=value, the value up to the next
//     '&', ';' or '#'.
//   - libpq key/value: host=h password=secret (spaces around '=', single
//     quotes and backslash escapes are understood).
//   - ADO/ODBC: Server=h;User Id=u;Password=secret (a value quoted with "
//     or ' or in {braces} may hold ';').
//
// A key is a password when it ends in "password" or is exactly pwd,
// passwd, pass or _auth_pass (any case). A DSN without a password is
// returned unchanged.
func RedactDSN(dsn string) string {
	return maskSpans(dsn, dsnPasswordSpans(dsn))
}

// dsnPasswordSpans returns the value spans every reading of dsn takes for a
// password (see RedactDSN); they may overlap.
func dsnPasswordSpans(dsn string) [][2]int {
	spans := otherPasswordSpans(dsn)
	if sp, ok := userinfoSpan(dsn, append(paramSpans(dsn), queryPasswordSpans(dsn)...)); ok {
		spans = append(spans, sp)
	}
	return spans
}

// otherPasswordSpans is every reading but the userinfo one: parameters,
// the URL query, libpq key/value and ADO/ODBC.
func otherPasswordSpans(dsn string) [][2]int {
	spans := paramSpans(dsn)
	spans = append(spans, queryPasswordSpans(dsn)...)
	for _, p := range kvPairs(dsn) {
		spans = append(spans, [2]int{p.vs, p.ve})
	}
	for _, p := range adoPairs(dsn) {
		spans = append(spans, [2]int{p.vs, p.ve})
	}
	return spans
}

// userinfoSpan is the lenient user:password@ reading, used by RedactDSN,
// RedactMessage and HasPassword: from the first ':' (after "scheme://" in a
// URL) to the last '@' outside a password parameter. Without "://" the
// part before the ':' must hold no '=', space or '(' (u@tcp(h:3306) is a
// user and an address), and odbc: strings and path DSNs (file:, sqlite:,
// sqlite3:, duckdb:) have no userinfo. It over-reads some '@'s in URL query
// values (postgres://u@h:5432/d?app=me@x), which is preferred to missing a
// password: StripPassword decides with the strict reading
// (strictUserinfo). params are the password-parameter spans.
func userinfoSpan(dsn string, params [][2]int) ([2]int, bool) {
	start := 0
	url := isURL(dsn)
	if url {
		start = len(Scheme(dsn)) + 3
	} else if sc := Scheme(dsn); sc == "odbc" || pathSchemes[sc] {
		return [2]int{}, false
	}
	at := -1
	for i := len(dsn) - 1; i >= start; i-- {
		if dsn[i] == '@' && !inSpans(i, params) {
			at = i
			break
		}
	}
	colon := strings.IndexByte(dsn[start:], ':')
	if at < 0 || colon < 0 || start+colon > at {
		return [2]int{}, false
	}
	colon += start
	if !url && strings.ContainsAny(dsn[:colon], "= \t\r\n(") {
		return [2]int{}, false
	}
	return [2]int{colon + 1, at}, true
}

// pathSchemes name DSNs that are a file path (sqlite:/tmp/a@b.db,
// file:x.db?…), which never carry a user:password@ part.
var pathSchemes = map[string]bool{"file": true, "sqlite": true, "sqlite3": true, "duckdb": true}

// isURL reports whether dsn is scheme://… .
func isURL(dsn string) bool {
	sc := Scheme(dsn)
	return sc != "" && len(dsn) >= len(sc)+3 && strings.EqualFold(dsn[len(sc):len(sc)+3], "://")
}

// userinfoState is what the strict userinfo reading concludes.
type userinfoState int

const (
	userinfoNone      userinfoState = iota // no user:password@ part
	userinfoFound                          // a password span StripPassword can cut cleanly
	userinfoAmbiguous                      // the lenient reading finds one the strict reading cannot place
)

// strictUserinfo is StripPassword's userinfo reading. It only reports a
// span it can cut without corrupting the DSN:
//
//   - URL: when the authority (up to the first '/', '?' or '#') holds a
//     '@', its userinfo is everything before the last one, and the password
//     follows its first ':' (myuser@server:pw@host); none without a ':'.
//     A '@' in the path then makes it ambiguous. Without a '@' in the
//     authority, a lenient span ending before the query is found
//     (u:p/ss@h/db); one ending in the query is ambiguous
//     (h:5432/db?app=svc@prod, u:p?ss@h).
//   - Otherwise: go-sql-driver's user:password@net(addr)/ or @/, with the
//     rightmost such '@' (user:pw@tcp(h)/d?loc=x@y). Failing that, a ':'
//     followed by '/' is a path (sqlite:/tmp/a@b.db): none. Anything else
//     the lenient reading finds is ambiguous.
func strictUserinfo(dsn string) ([2]int, userinfoState) {
	params := append(paramSpans(dsn), queryPasswordSpans(dsn)...)
	lenient, ok := userinfoSpan(dsn, params)
	if isURL(dsn) {
		start := len(Scheme(dsn)) + 3
		authEnd := len(dsn)
		if i := strings.IndexAny(dsn[start:], "/?#"); i >= 0 {
			authEnd = start + i
		}
		pathEnd := len(dsn)
		if i := strings.IndexAny(dsn[authEnd:], "?#"); i >= 0 {
			pathEnd = authEnd + i
		}
		if at := strings.LastIndexByte(dsn[start:authEnd], '@'); at >= 0 {
			if strings.Contains(dsn[authEnd:pathEnd], "@") {
				return [2]int{}, userinfoAmbiguous
			}
			if c := strings.IndexByte(dsn[start:start+at], ':'); c >= 0 {
				return [2]int{start + c + 1, start + at}, userinfoFound
			}
			if ok && lenient[1] > pathEnd { // the lenient '@' sits in the query
				return [2]int{}, userinfoNone
			}
			if ok {
				return [2]int{}, userinfoAmbiguous
			}
			return [2]int{}, userinfoNone
		}
		switch {
		case !ok:
			return [2]int{}, userinfoNone
		case lenient[1] < pathEnd:
			return lenient, userinfoFound
		}
		return [2]int{}, userinfoAmbiguous
	}
	if !ok {
		return [2]int{}, userinfoNone
	}
	colon := lenient[0] - 1
	if !strings.ContainsAny(dsn[:colon], "/(") {
		for i := len(dsn) - 1; i > colon; i-- {
			if dsn[i] == '@' && !inSpans(i, params) && goSQLAddr(dsn[i+1:]) {
				return [2]int{colon + 1, i}, userinfoFound
			}
		}
	}
	if colon+1 < len(dsn) && dsn[colon+1] == '/' {
		return [2]int{}, userinfoNone
	}
	return [2]int{}, userinfoAmbiguous
}

// goSQLAddr reports whether s, the text after a go-sql-driver DSN's '@',
// starts with net(addr) (the net name may hold '-', '_' and '.', as in
// cloudsql-mysql, or be empty) or '/'.
func goSQLAddr(s string) bool {
	if strings.HasPrefix(s, "/") {
		return true
	}
	i := 0
	for i < len(s) && (isKeyChar(s[i])) {
		i++
	}
	return i < len(s) && s[i] == '('
}

func inSpans(i int, spans [][2]int) bool {
	return slices.ContainsFunc(spans, func(sp [2]int) bool { return sp[0] <= i && i < sp[1] })
}

// queryPasswordSpans is the URL query reading: in a DSN with a scheme, the
// value of a password parameter of its query runs to the next '&' or '#'
// only. Go's URL parser does not split on ';', so a ';' may belong to the
// password (?password=Se;cret); this reading masks it with the rest.
func queryPasswordSpans(dsn string) [][2]int {
	q := strings.IndexByte(dsn, '?')
	if q < 0 || Scheme(dsn) == "" {
		return nil
	}
	var spans [][2]int
	for i := q; i < len(dsn); i++ {
		if c := dsn[i]; c != '?' && c != '&' {
			continue
		}
		ks := i + 1
		ke := ks
		for ke < len(dsn) && !strings.ContainsRune("=&#", rune(dsn[ke])) {
			ke++
		}
		if ke >= len(dsn) || dsn[ke] != '=' || !isPasswordKey(dsn[ks:ke]) {
			continue
		}
		ve := ke + 1
		for ve < len(dsn) && dsn[ve] != '&' && dsn[ve] != '#' {
			ve++
		}
		spans = append(spans, [2]int{ke + 1, ve})
		i = ve - 1
	}
	return spans
}

// paramSpans returns the value spans of every [?&;]key=value pair of s whose
// key names a password; a value runs to the next '&', ';' or '#'.
func paramSpans(s string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '?' && c != '&' && c != ';' {
			continue
		}
		ks := i + 1
		ke := ks
		for ke < len(s) && !strings.ContainsRune("=&;#?", rune(s[ke])) {
			ke++
		}
		if ke >= len(s) || s[ke] != '=' || !isPasswordKey(s[ks:ke]) {
			continue
		}
		ve := ke + 1
		for ve < len(s) && !strings.ContainsRune("&;#", rune(s[ve])) {
			ve++
		}
		spans = append(spans, [2]int{ke + 1, ve})
		i = ve - 1
	}
	return spans
}

// pair locates one password key and its value in a DSN: the key starts at
// ks, the value spans [vs, ve).
type pair struct{ ks, vs, ve int }

// kvPairs returns the password pairs of a libpq-style "k=v k='v w'" string.
func kvPairs(dsn string) []pair {
	var out []pair
	i, n := 0, len(dsn)
	for i < n {
		if isSpace(dsn[i]) {
			i++
			continue
		}
		ks := i
		for i < n && dsn[i] != '=' && !isSpace(dsn[i]) {
			i++
		}
		key := dsn[ks:i]
		j := i
		for j < n && isSpace(dsn[j]) {
			j++
		}
		if j >= n || dsn[j] != '=' {
			continue
		}
		j++
		for j < n && isSpace(dsn[j]) {
			j++
		}
		i = j
		if i < n && dsn[i] == '\'' {
			i++
			for i < n && dsn[i] != '\'' {
				if dsn[i] == '\\' && i+1 < n {
					i++
				}
				i++
			}
			if i < n {
				i++
			}
		} else {
			for i < n && !isSpace(dsn[i]) {
				if dsn[i] == '\\' && i+1 < n {
					i++
				}
				i++
			}
		}
		if isPasswordKey(key) && isKey(key) {
			out = append(out, pair{ks, j, min(i, n)})
		}
	}
	return out
}

// isKey reports whether k is a plain libpq keyword (letters, digits, '_',
// '-', '.'), so the libpq reading does not take a URL's
// "scheme://h/db?sslpassword" for a key and swallow the next parameters.
func isKey(k string) bool {
	for i := 0; i < len(k); i++ {
		if !isKeyChar(k[i]) {
			return false
		}
	}
	return k != ""
}

// adoPairs returns the password pairs (Password=, Pwd=, any case) of an
// ADO/ODBC-style "Key=value;Key=value" string; a string without ';' has
// none. A value quoted with " or ' runs to its closing quote (a doubled
// quote escapes one), and one in braces runs to the matching '}' ("}}"
// escapes one), so either may hold a ';'. ks is the segment's start (just
// past the previous ';'), and the value span covers the whole value,
// spaces included, up to the next ';'.
func adoPairs(dsn string) []pair {
	if !strings.Contains(dsn, ";") {
		return nil
	}
	var out []pair
	i, n := 0, len(dsn)
	for i < n {
		eq := i
		for eq < n && dsn[eq] != '=' && dsn[eq] != ';' {
			eq++
		}
		if eq >= n || dsn[eq] == ';' {
			i = eq + 1
			continue
		}
		key := strings.TrimSpace(dsn[i:eq])
		vs := eq + 1
		j := vs
		for j < n && isSpace(dsn[j]) {
			j++
		}
		if j < n {
			j = quotedEnd(dsn, j)
		}
		for j < n && dsn[j] != ';' {
			j++
		}
		if isPasswordKey(key) {
			out = append(out, pair{i, vs, j})
		}
		i = j + 1
	}
	return out
}

// quotedEnd returns the index just past the ADO/ODBC value starting at j
// when it is quoted ("…", '…' with the quote doubled to escape it, or {…}
// with "}}" escaping '}'), and j otherwise. An unterminated value runs to
// the end.
func quotedEnd(s string, j int) int {
	var closing byte
	switch s[j] {
	case '"', '\'':
		closing = s[j]
	case '{':
		closing = '}'
	default:
		return j
	}
	for j++; j < len(s); j++ {
		if s[j] == closing {
			if j+1 < len(s) && s[j+1] == closing {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

// StripPassword returns dsn without its password, in every form RedactDSN
// understands: the userinfo password and password parameters of a URL
// (other parameters keep their separators), those parameters in an opaque
// URI (sqlite:x.db?_auth_pass=…), the user:password@ of a MySQL DSN, and
// the password pairs of a libpq key=value or ADO/ODBC Key=value; string,
// removed with their key. had reports whether dsn carried a password. When
// none can be removed safely (one is left, or the libpq and ADO readings
// disagree so that nothing safe remains), the result is "" with had true:
// the caller must not use the DSN.
func StripPassword(dsn string) (stripped string, had bool) {
	if !HasPassword(dsn) {
		return dsn, false
	}
	sp, st := strictUserinfo(dsn)
	_, lenient := userinfoSpan(dsn, append(paramSpans(dsn), queryPasswordSpans(dsn)...))
	// The lenient reading finds a password the strict one cannot place.
	if st == userinfoAmbiguous || st == userinfoNone && lenient {
		return "", true
	}
	out := dsn
	if st == userinfoFound {
		out = cutUserinfo(dsn, sp)
	}
	if isURL(dsn) || st == userinfoFound || Scheme(dsn) != "" {
		var ok bool
		if out, ok = stripQuery(out); !ok {
			return "", true
		}
	} else {
		out = stripPairs(out)
	}
	// Final invariants: no lenient reading finds a password in the output
	// (RedactDSN(out) == out, which HasPassword includes), and no value any
	// reading found in dsn survives.
	if out == "" || HasPassword(out) || RedactDSN(out) != out || keepsPassword(dsn, out) {
		return "", true
	}
	return out, true
}

// cutUserinfo removes ":password" (the span sp and its ':') from dsn, and
// the '@' too when the user is empty (scheme://:password@host, :pw@…).
func cutUserinfo(dsn string, sp [2]int) string {
	colon, at := sp[0]-1, sp[1]
	if colon == 0 || dsn[colon-1] == '/' {
		return dsn[:colon] + dsn[at+1:]
	}
	return dsn[:colon] + dsn[at:]
}

// keepsPassword reports whether out still holds a password value any
// reading finds in dsn. StripPassword rewrites with one reading; this
// check, over all of them, makes sure it never returns a password another
// reading saw.
func keepsPassword(dsn, out string) bool {
	for _, pw := range rawDSNPasswords(dsn) {
		if pw != "" && pw != maskedPassword && strings.Contains(out, pw) {
			return true
		}
	}
	return false
}

// stripQuery drops the password parameters from the query of s, keeping the
// other parameters with the separator ('&' or ';') each had. A password
// value followed by ';' segments without '=' (?password=Se;cret) loses them
// too, since Go's URL parser does not split on ';'. When a ';' segment with
// '=' follows a password (?password=Se;a=cret), the password cannot be told
// from a parameter and ok is false: the caller must refuse.
func stripQuery(s string) (out string, ok bool) {
	q := strings.IndexByte(s, '?')
	if q < 0 {
		return s, true
	}
	query, frag := s[q+1:], ""
	if h := strings.IndexByte(query, '#'); h >= 0 {
		query, frag = query[:h], query[h:]
	}
	var b strings.Builder
	sep := byte('?')
	for len(query) > 0 || sep != 0 {
		e := strings.IndexAny(query, "&;")
		param, next := query, byte(0)
		if e >= 0 {
			param, next = query[:e], query[e]
			query = query[e+1:]
		} else {
			query = ""
		}
		k, _, _ := strings.Cut(param, "=")
		if param != "" && isPasswordKey(k) {
			// ?password=Se;cret: a ';' segment without '=' is still the
			// password (Go's URL parser does not split on ';').
			for next == ';' {
				e := strings.IndexAny(query, "&;")
				seg := query
				if e >= 0 {
					seg = query[:e]
				}
				if strings.Contains(seg, "=") {
					return "", false
				}
				if e < 0 {
					query, next = "", 0
				} else {
					next, query = query[e], query[e+1:]
				}
			}
		}
		if param != "" && !isPasswordKey(k) {
			if b.Len() == 0 {
				b.WriteByte('?')
			} else {
				b.WriteByte(sep)
			}
			b.WriteString(param)
		}
		sep = next
		if next == 0 {
			break
		}
	}
	return s[:q] + b.String() + frag, true
}

// stripPairs removes the password pairs of a libpq or ADO/ODBC key=value
// string. The libpq reading is used unless the ADO reading finds a password
// value it does not cover (Server=h; Password = a b ;…), so a libpq quoted
// value holding a ';' survives intact.
func stripPairs(dsn string) string {
	kv, ado := kvPairs(dsn), adoPairs(dsn)
	if len(kv) > 0 && covered(ado, kv) {
		var b strings.Builder
		pos := 0
		for _, p := range kv {
			b.WriteString(strings.TrimRight(dsn[pos:p.ks], " \t\r\n"))
			pos = p.ve
		}
		b.WriteString(dsn[pos:])
		return strings.TrimSpace(b.String())
	}
	var b strings.Builder
	pos := 0
	for _, p := range ado {
		end := p.ve
		if end < len(dsn) {
			end++ // the ';' after the value
		}
		start := p.ks
		if end == len(dsn) && start > 0 && dsn[end-1] != ';' {
			start-- // the last segment: drop the ';' before it instead
		}
		b.WriteString(dsn[pos:start])
		pos = end
	}
	b.WriteString(dsn[pos:])
	return b.String()
}

// covered reports whether every value span of ado lies within one of kv.
func covered(ado, kv []pair) bool {
	for _, a := range ado {
		if !slices.ContainsFunc(kv, func(k pair) bool { return k.vs <= a.vs && a.ve <= k.ve }) {
			return false
		}
	}
	return true
}

// maskSpans replaces each span of s (overlapping ones merged) with the
// placeholder.
func maskSpans(s string, spans [][2]int) string {
	if len(spans) == 0 {
		return s
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var b strings.Builder
	pos := 0
	for k := 0; k < len(spans); {
		st, en := spans[k][0], spans[k][1]
		for k++; k < len(spans) && spans[k][0] <= en; k++ {
			en = max(en, spans[k][1])
		}
		b.WriteString(s[pos:st])
		b.WriteString(maskedPassword)
		pos = en
	}
	b.WriteString(s[pos:])
	return b.String()
}

// secretKeys are the parameter names, besides the "*password" suffix, that
// hold a secret. They match the whole name: "keyspace" is not "key".
var secretKeys = map[string]bool{
	"pwd": true, "passwd": true, "pass": true, "_auth_pass": true,
	"credentials": true, "credentials_json": true,
	"private_key": true, "privatekey": true,
	"token": true, "auth_token": true, "authtoken": true, "access_token": true,
	"secret": true, "client_secret": true,
	"api_key": true, "apikey": true, "api-key": true,
	"key": true, "account_key": true, "accountkey": true,
	"sas": true, "sig": true,
}

// isPasswordKey reports whether k names a secret: any key ending in
// "password", or exactly one of secretKeys (pwd, passwd, pass, SQLite's
// _auth_pass, credentials, private_key, token, secret, api_key, key,
// account_key, sas, sig and their variants), case-insensitive.
func isPasswordKey(k string) bool {
	k = strings.ToLower(k)
	return secretKeys[k] || strings.HasSuffix(k, "password")
}

// isStrictPasswordKey reports whether k names a password proper: a key
// ending in "password", or pwd, passwd, pass or _auth_pass. Free-form
// messages mask these in every form; the other secret keys only as
// key=value (see passwordSpans).
func isStrictPasswordKey(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "pwd", "passwd", "pass", "_auth_pass":
		return true
	}
	return strings.HasSuffix(k, "password")
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// RedactMessage masks credentials in a free-form message, typically an error
// text, before it is printed. It masks, in order:
//
//   - the literal cfg.DSN, replaced by RedactDSN(cfg.DSN), and every bare
//     occurrence of a password embedded in cfg.DSN;
//   - cfg.Password and its URL-escaped forms, when the password is non-empty;
//   - the password of every scheme://user:password@ URL found in msg;
//   - the value of every key=value pair, YAML "key: value" or JSON
//     "key":"value" whose key is a password key (ending in "password", or
//     pwd, passwd, pass, _auth_pass; case-insensitive, spaces around the
//     separator and quoted values understood);
//   - the value of every key=value pair (the form of a DSN, a URL query or
//     an ADO string) whose key is one of the other secret keys (token,
//     secret, sig, sas, credentials, api_key, key …). As a JSON field or
//     YAML "key: value" these are left alone: in a watched change event or
//     a stored JSON query they are data, not a connection secret.
//
// Bare occurrences of a password are masked only when it is at least 4 bytes
// long; shorter ones are masked in the structured positions above only.
// Masked text reads "xxxxx". Over-redaction is preferred to leaking.
func RedactMessage(msg string, cfg ConnConfig) string {
	if cfg.DSN != "" {
		msg = strings.ReplaceAll(msg, cfg.DSN, RedactDSN(cfg.DSN))
		pws := dsnPasswords(cfg.DSN)
		// Longest first: one reading's value can be a prefix of another's
		// ({a};b} read up to the ';' by the parameter reading).
		slices.SortStableFunc(pws, func(a, b string) int { return len(b) - len(a) })
		for _, pw := range pws {
			if len(pw) >= minBarePassword {
				msg = strings.ReplaceAll(msg, pw, maskedPassword)
			}
		}
	}
	if pw := cfg.Password.Reveal(); pw != "" {
		escaped := strings.TrimPrefix(url.UserPassword("", pw).String(), ":")
		for _, form := range []string{pw, url.QueryEscape(pw), url.PathEscape(pw), escaped} {
			if len(form) >= minBarePassword {
				msg = strings.ReplaceAll(msg, form, maskedPassword)
			}
		}
	}
	return maskKeyValues(maskEmbeddedUserinfo(msg))
}

// maskEmbeddedUserinfo masks the userinfo password of every "scheme://" URL
// in s. A URL runs to the next whitespace or quote character.
func maskEmbeddedUserinfo(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		start := i + 3
		end := start
		for end < len(s) && !isSpace(s[end]) && !strings.ContainsRune("\"'`<>", rune(s[end])) {
			end++
		}
		tok := s[start:end]
		if at := strings.LastIndexByte(tok, '@'); at >= 0 {
			if colon := strings.IndexByte(tok, ':'); colon >= 0 && colon < at {
				tok = tok[:colon+1] + maskedPassword + tok[at:]
			}
		}
		b.WriteString(s[:start])
		b.WriteString(tok)
		s = s[end:]
	}
}

// maskKeyValues masks the value of every secret "<key>=<value>" in s
// (see passwordSpans for the keys and forms).
func maskKeyValues(s string) string {
	spans := passwordSpans(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	i := 0
	for _, sp := range spans {
		b.WriteString(s[i:sp[0]])
		b.WriteString(maskedPassword)
		i = sp[1]
	}
	b.WriteString(s[i:])
	return b.String()
}

// passwordSpans returns the [start, end) spans of the values of every
// "<key>=<value>", "<key>: <value>" and "\"<key>\":<value>" in s whose key
// is a password key, and of every "<key>=<value>" whose key is another
// secret key (isPasswordKey). Keys are runs of letters, digits, '_', '-' and '.',
// optionally quoted (JSON) and followed by spaces; a value is a quoted string
// or runs to the next whitespace, quote, '&', ';' or '#'. An unquoted
// structured value is not cut at ',' or a closing bracket, since a password
// may contain them: "pwd":12345,"x":1 masks "12345,". A key with an empty
// value yields no span.
func passwordSpans(s string) [][2]int {
	var spans [][2]int
	i := 0
	for i < len(s) {
		sep := strings.IndexAny(s[i:], "=:")
		if sep < 0 {
			return spans
		}
		sep += i
		i = sep + 1
		colon := s[sep] == ':'
		if colon && strings.HasPrefix(s[sep:], "://") {
			continue
		}
		ke := sep
		for ke > 0 && s[ke-1] == ' ' {
			ke--
		}
		if colon && ke > 0 && (s[ke-1] == '"' || s[ke-1] == '\'') {
			ke--
		}
		ks := ke
		for ks > 0 && isKeyChar(s[ks-1]) {
			ks--
		}
		if ks == ke || !isPasswordKey(s[ks:ke]) {
			continue
		}
		// The secret keys that are not passwords (token, secret, sig, sas,
		// credentials, api_key, key …) are masked only as key=value, the
		// form of a DSN, a URL query or an ADO string. As a JSON field or
		// a YAML "name: value" they are far more often a record's data (a
		// watched change event, a stored JSON query) than a secret.
		if colon && !isStrictPasswordKey(s[ks:ke]) {
			continue
		}
		vs := sep + 1
		for vs < len(s) && s[vs] == ' ' {
			vs++
		}
		// In a URL query ("?k=v", "&k=v") a real ':' in a value is normally
		// percent-encoded, so a trailing one is the message's punctuation.
		// Anywhere else it may be the password's last byte: keep it in.
		i = valueEnd(s, vs, ks > 0 && (s[ks-1] == '?' || s[ks-1] == '&'))
		if i > vs { // "enter password:" has no value to mask
			spans = append(spans, [2]int{vs, i})
		}
	}
	return spans
}

// HasPassword reports whether dsn carries a password: RedactDSN would change
// it, or its password is the placeholder "xxxxx" RedactDSN writes (a redacted
// DSN that was saved back must not pass for a clean one).
func HasPassword(dsn string) bool {
	if RedactDSN(dsn) != dsn {
		return true
	}
	return slices.Contains(rawDSNPasswords(dsn), maskedPassword)
}

// dsnPasswords returns the passwords embedded in dsn: the userinfo password
// of a URL or user:password@ DSN and every password-keyed value, each also
// in its unescaped forms. Empty values and the placeholder are skipped.
func dsnPasswords(dsn string) []string {
	var out []string
	for _, v := range rawDSNPasswords(dsn) {
		if v == "" || v == maskedPassword {
			continue
		}
		out = append(out, v)
		if u, err := url.PathUnescape(v); err == nil && u != v {
			out = append(out, u)
		}
		if u, err := url.QueryUnescape(v); err == nil && u != v {
			out = append(out, u)
		}
	}
	return out
}

// rawDSNPasswords returns the unquoted password values in dsn, whatever they
// are: every span any RedactDSN reading finds, plus the message-style
// key=value spans of passwordSpans.
func rawDSNPasswords(dsn string) []string {
	var out []string
	for _, sp := range dsnPasswordSpans(dsn) {
		out = append(out, unquoteValue(dsn[sp[0]:sp[1]]))
	}
	for _, sp := range passwordSpans(dsn) {
		out = append(out, unquoteValue(dsn[sp[0]:sp[1]]))
	}
	return out
}

// unquoteValue trims a password value and undoes its '…', "…" or {…}
// quoting.
func unquoteValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '{' && v[len(v)-1] == '}' {
		v = strings.ReplaceAll(v[1:len(v)-1], "}}", "}")
	}
	return v
}

// valueEnd returns the index just past the value starting at i. With
// queryParam, trailing ':' before whitespace is left out of an unquoted value
// ("…?_auth_pass=X: no such file"); otherwise the value keeps it, so a
// password ending in ':' is masked whole.
func valueEnd(s string, i int, queryParam bool) int {
	if i < len(s) && s[i] == '{' {
		return quotedEnd(s, i)
	}
	if i < len(s) && (s[i] == '\'' || s[i] == '"') {
		q := s[i]
		for i++; i < len(s) && s[i] != q; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
		}
		return min(i+1, len(s))
	}
	start := i
	for i < len(s) && !isSpace(s[i]) && !strings.ContainsRune("&;#\"'`", rune(s[i])) {
		i++
	}
	// "…?_auth_pass=X: no such file": the colon belongs to the message.
	if queryParam && i < len(s) && isSpace(s[i]) {
		for i > start+1 && s[i-1] == ':' {
			i--
		}
	}
	return i
}

func isKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}
