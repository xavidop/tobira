package redis

import (
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// renderReply turns a RESP2 reply into a table. Commands whose reply has a
// known shape get named columns:
//
//	HGETALL, HSCAN          field, value
//	MGET                    key, value (zipped with the arguments)
//	HMGET                   field, value
//	Z*RANGE* WITHSCORES     member, score
//	ZSCAN                   member, score
//	SSCAN                   member
//	SCAN                    key
//	XRANGE, XREVRANGE       id, fields
//	INFO                    section, key, value
//	FT.SEARCH               _key, [_score,] fields... (typed as records)
//
// Any other array is one row per element in a "value" column; a scalar is
// one row. For the scans, the next cursor is returned as a notice.
func renderReply(args []string, reply any) ([]driver.Column, []driver.Row, []string) {
	name, _ := canonicalName(args)
	arr, isArr := reply.([]any)
	switch {
	case name == "INFO":
		if s, ok := reply.(string); ok {
			return renderInfo(s)
		}
	case !isArr:
	case name == "CONFIG GET":
		cols, rows, notes := pairs(arr, "parameter", "value", false)
		for _, r := range rows {
			if k, _ := r[0].AsText(); secretParam(k) && !r[1].IsNull() {
				r[1] = driver.String(masked)
			}
		}
		return cols, rows, notes
	case name == "ACL LIST" || name == "ACL GETUSER":
		reply = maskACL(arr)
		arr = reply.([]any)
	case name == "HGETALL":
		return pairs(arr, "field", "value", false)
	case name == "MGET" && len(arr) == len(args)-1:
		return zipArgs(args[1:], arr, "key")
	case name == "HMGET" && len(arr) == len(args)-2:
		return zipArgs(args[2:], arr, "field")
	case (name == "ZRANGE" || name == "ZRANGEBYSCORE" || name == "ZREVRANGE") && hasArg(args, "WITHSCORES"):
		return pairs(arr, "member", "score", true)
	case name == "SCAN" || name == "HSCAN" || name == "SSCAN" || name == "ZSCAN":
		return renderScan(name, arr)
	case name == "XRANGE" || name == "XREVRANGE":
		return renderStream(arr)
	case name == "FT.SEARCH":
		if cols, rows, ok := renderSearch(args, arr); ok {
			return cols, rows, nil
		}
	}
	var rows []driver.Row
	if !isArr {
		rows = []driver.Row{{replyValue(reply)}}
	} else {
		rows = make([]driver.Row, len(arr))
		for i, e := range arr {
			rows[i] = driver.Row{replyValue(e)}
		}
	}
	return []driver.Column{{Name: "value", NativeType: "reply", Type: valueColumnType(rows)}}, rows, nil
}

// valueColumnType is the natural type of a one-column reply: the type all
// non-null values share (a GET is a string, an INCR an int), string when
// every value is nil (a nil bulk string), and unknown when they differ.
func valueColumnType(rows []driver.Row) driver.Type {
	t := driver.TypeNull
	for _, r := range rows {
		switch vt := r[0].Type(); {
		case vt == driver.TypeNull:
		case t == driver.TypeNull:
			t = vt
		case vt != t:
			return driver.TypeUnknown
		}
	}
	if t == driver.TypeNull {
		return driver.TypeString
	}
	return t
}

// masked replaces a secret in a reply.
const masked = "xxxxx"

// secretParam reports whether a CONFIG parameter holds a secret:
// requirepass, masterauth, and any name containing "pass" or "secret".
func secretParam(name string) bool {
	l := strings.ToLower(name)
	return l == "requirepass" || l == "masterauth" || strings.Contains(l, "pass") || strings.Contains(l, "secret")
}

// maskACL masks password material in an ACL LIST or ACL GETUSER reply: the
// list after "passwords", and every #hash, >password and <password token
// of a rule string.
func maskACL(x any) any {
	switch v := x.(type) {
	case string:
		return maskACLRule(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			if i > 0 && v[i-1] == "passwords" {
				if list, ok := e.([]any); ok {
					m := make([]any, len(list))
					for j := range m {
						m[j] = masked
					}
					out[i] = m
					continue
				}
			}
			out[i] = maskACL(e)
		}
		return out
	}
	return x
}

func maskACLRule(rule string) string {
	fields := strings.Split(rule, " ")
	for i, f := range fields {
		if f != "" && (f[0] == '#' || f[0] == '>' || f[0] == '<') {
			fields[i] = f[:1] + masked
		}
	}
	return strings.Join(fields, " ")
}

func hasArg(args []string, want string) bool {
	for _, a := range args[1:] {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// replyValue converts one RESP2 element: bulk strings to String (Bytes when
// not UTF-8), integers to Int, nil to NULL, arrays to List and an error
// element to its text.
func replyValue(x any) driver.Value {
	switch v := x.(type) {
	case nil:
		return driver.Null()
	case string:
		return textValue(v)
	case int64:
		return driver.Int(v)
	case []any:
		vs := make([]driver.Value, len(v))
		for i, e := range v {
			vs[i] = replyValue(e)
		}
		return driver.List(vs...)
	case error:
		return driver.String(v.Error())
	}
	val, err := driver.FromAny(x)
	if err != nil {
		return driver.String("?")
	}
	return val
}

// scoreValue shows a sorted-set score as a Float, or its text when it does
// not parse (inf scores parse; anything else is kept as written).
func scoreValue(x any) driver.Value {
	if s, ok := x.(string); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return driver.Float(f)
		}
	}
	return replyValue(x)
}

func pairs(arr []any, k, v string, score bool) ([]driver.Column, []driver.Row, []string) {
	cols := []driver.Column{{Name: k, Type: driver.TypeString}, {Name: v, Type: driver.TypeString}}
	if score {
		cols[1].Type = driver.TypeFloat
	}
	rows := make([]driver.Row, 0, len(arr)/2)
	for i := 0; i+1 < len(arr); i += 2 {
		val := replyValue(arr[i+1])
		if score {
			val = scoreValue(arr[i+1])
		}
		rows = append(rows, driver.Row{replyValue(arr[i]), val})
	}
	return cols, rows, nil
}

func zipArgs(names []string, arr []any, k string) ([]driver.Column, []driver.Row, []string) {
	cols := []driver.Column{{Name: k, Type: driver.TypeString}, {Name: "value", Type: driver.TypeString}}
	rows := make([]driver.Row, len(arr))
	for i, e := range arr {
		rows[i] = driver.Row{textValue(names[i]), replyValue(e)}
	}
	return cols, rows, nil
}

func renderScan(name string, arr []any) ([]driver.Column, []driver.Row, []string) {
	if len(arr) != 2 {
		return renderReply([]string{"?"}, arr)
	}
	items, _ := arr[1].([]any)
	var notices []string
	if cur, ok := arr[0].(string); ok {
		notices = []string{"next cursor: " + cur}
	}
	var cols []driver.Column
	var rows []driver.Row
	switch name {
	case "HSCAN":
		cols, rows, _ = pairs(items, "field", "value", false)
	case "ZSCAN":
		cols, rows, _ = pairs(items, "member", "score", true)
	default:
		col := "key"
		if name == "SSCAN" {
			col = "member"
		}
		cols = []driver.Column{{Name: col, Type: driver.TypeString}}
		for _, e := range items {
			rows = append(rows, driver.Row{replyValue(e)})
		}
	}
	return cols, rows, notices
}

func renderStream(arr []any) ([]driver.Column, []driver.Row, []string) {
	cols := []driver.Column{{Name: "id", Type: driver.TypeString}, {Name: "fields", Type: driver.TypeObject}}
	rows := make([]driver.Row, 0, len(arr))
	for _, e := range arr {
		entry, ok := e.([]any)
		if !ok || len(entry) != 2 {
			rows = append(rows, driver.Row{replyValue(e), driver.Null()})
			continue
		}
		fv, _ := entry[1].([]any)
		rows = append(rows, driver.Row{replyValue(entry[0]), objectFromPairs(fv)})
	}
	return cols, rows, nil
}

// objectFromPairs turns a flat field/value array into an Object.
func objectFromPairs(fv []any) driver.Value {
	ms := make([]driver.Member, 0, len(fv)/2)
	for i := 0; i+1 < len(fv); i += 2 {
		k, _ := fv[i].(string)
		ms = append(ms, driver.Member{Key: k, Value: replyValue(fv[i+1])})
	}
	return driver.ObjectValue(ms...)
}

func renderInfo(s string) ([]driver.Column, []driver.Row, []string) {
	cols := []driver.Column{{Name: "section", Type: driver.TypeString}, {Name: "key", Type: driver.TypeString}, {Name: "value", Type: driver.TypeString}}
	var rows []driver.Row
	section := ""
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			section = strings.TrimSpace(line[1:])
		default:
			k, v, _ := strings.Cut(line, ":")
			rows = append(rows, driver.Row{driver.String(section), driver.String(k), driver.String(v)})
		}
	}
	return cols, rows, nil
}

// renderSearch renders an FT.SEARCH reply: [total, key, (score,) [f, v, ...], ...].
// NOCONTENT replies have keys only. Fields are typed as records.
func renderSearch(args []string, arr []any) ([]driver.Column, []driver.Row, bool) {
	if len(arr) == 0 {
		return nil, nil, false
	}
	if _, ok := arr[0].(int64); !ok {
		return nil, nil, false
	}
	withScores := hasArg(args, "WITHSCORES")
	noContent := hasArg(args, "NOCONTENT")
	var keys []string
	var scores []any
	var recs []map[string]string
	var names []string
	seen := map[string]bool{}
	for i := 1; i < len(arr); {
		k, ok := arr[i].(string)
		if !ok {
			return nil, nil, false
		}
		i++
		keys = append(keys, k)
		if withScores && i < len(arr) {
			scores = append(scores, arr[i])
			i++
		}
		rec := map[string]string{}
		if !noContent && i < len(arr) {
			fv, ok := arr[i].([]any)
			if !ok {
				return nil, nil, false
			}
			i++
			for j := 0; j+1 < len(fv); j += 2 {
				f, _ := fv[j].(string)
				v, ok := fv[j+1].(string)
				if !ok {
					continue
				}
				rec[f] = v
				if !seen[f] {
					seen[f] = true
					names = append(names, f)
				}
			}
		}
		recs = append(recs, rec)
	}
	fields := inferFields(names, recs)
	cols := []driver.Column{{Name: "_key", Type: driver.TypeString, NativeType: "key"}}
	if withScores {
		cols = append(cols, driver.Column{Name: "_score", Type: driver.TypeFloat})
	}
	for _, f := range fields {
		cols = append(cols, driver.Column{Name: f.Name, NativeType: f.NativeType, Type: f.Type})
	}
	rows := make([]driver.Row, len(keys))
	for r, k := range keys {
		row := driver.Row{textValue(k)}
		if withScores {
			row = append(row, scoreValue(scores[r]))
		}
		for _, f := range fields {
			if v, ok := recs[r][f.Name]; ok {
				row = append(row, typedValue(v, f.Type))
			} else {
				row = append(row, driver.Null())
			}
		}
		rows[r] = row
	}
	return cols, rows, true
}
