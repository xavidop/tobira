package redis

import (
	"context"
	"strings"
)

// allKeys is the collection name that holds every key of a database.
const allKeys = "*"

// escapeGlob escapes the SCAN MATCH metacharacters in s.
func escapeGlob(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// matchPattern is the SCAN MATCH pattern for a collection: "prefix:*".
// inCollection checks each key again.
func matchPattern(coll string) string {
	if coll == allKeys {
		return allKeys
	}
	return escapeGlob(coll) + ":*"
}

// inCollection reports whether key belongs to collection coll: the key
// starts with "prefix:". A bare key named like the prefix is in "*" only,
// as prefixCollections counts it. Collection "*" holds every key; the
// empty name holds none.
func inCollection(coll, key string) bool {
	if coll == allKeys {
		return true
	}
	return coll != "" && strings.HasPrefix(key, coll+":")
}

// globCheckEvery is how many matcher steps run between ctx checks.
const globCheckEvery = 4096

// globMatch reports whether s matches a Redis glob pattern (the
// stringmatchlen rules: * ? [abc] [^a] [a-z] and \ escapes). It is the
// iterative two-pointer matcher: on a mismatch it only returns to the last
// '*', so it runs in O(len(pat) * len(s)) time, never exponentially (the
// shape of CVE-2022-36021). ctx is checked every globCheckEvery steps.
func globMatch(ctx context.Context, pat, s string) (bool, error) {
	p, i := 0, 0
	star, mark := -1, 0
	for steps := 0; i < len(s); steps++ {
		if steps%globCheckEvery == globCheckEvery-1 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if p < len(pat) && pat[p] == '*' {
			for p < len(pat) && pat[p] == '*' {
				p++
			}
			if p == len(pat) {
				return true, nil
			}
			star, mark = p, i
			continue
		}
		if p < len(pat) {
			if n, ok := matchOne(pat[p:], s[i]); ok {
				p += n
				i++
				continue
			}
		}
		if star < 0 {
			return false, nil
		}
		mark++
		p, i = star, mark
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat), nil
}

// matchOne matches the single-byte pattern element at the start of pat (a
// literal, an escape, '?' or a class) against c, returning its length.
func matchOne(pat string, c byte) (int, bool) {
	switch pat[0] {
	case '?':
		return 1, true
	case '[':
		return matchClass(pat, c)
	case '\\':
		if len(pat) >= 2 {
			return 2, pat[1] == c
		}
	}
	return 1, pat[0] == c
}

// matchClass matches c against the [...] class at the start of pat and
// returns the index after the class.
func matchClass(pat string, c byte) (int, bool) {
	i := 1
	not := i < len(pat) && pat[i] == '^'
	if not {
		i++
	}
	match := false
	for i < len(pat) && pat[i] != ']' {
		switch {
		case pat[i] == '\\' && i+1 < len(pat):
			i++
			if pat[i] == c {
				match = true
			}
			i++
		case i+2 < len(pat) && pat[i+1] == '-' && pat[i+2] != ']':
			lo, hi := pat[i], pat[i+2]
			if lo > hi {
				lo, hi = hi, lo
			}
			if c >= lo && c <= hi {
				match = true
			}
			i += 3
		default:
			if pat[i] == c {
				match = true
			}
			i++
		}
	}
	if i < len(pat) {
		i++ // the closing ]
	}
	return i, match != not
}
