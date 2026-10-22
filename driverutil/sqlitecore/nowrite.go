package sqlitecore

import (
	"errors"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// errLoadExtension explains an ErrReadOnly for load_extension.
var errLoadExtension = errors.New("load_extension is not allowed on a read-only connection: a loaded extension outlives the rollback")

// errWritableCTE explains an ErrReadOnly from CheckNoWrite.
var errWritableCTE = errors.New("INSERT, UPDATE, DELETE and REPLACE are not allowed on a read-only connection, not even behind WITH")

// ReadOnlyGuard is the lexical gate for a server that cannot enforce
// read-only itself with PRAGMA query_only: the allow-list (CheckReadOnly),
// then CheckNoWrite, which refuses the writable CTE ("WITH … DELETE") that
// the allow-list lets through.
func ReadOnlyGuard(name, text string) error {
	if err := CheckReadOnly(name, text); err != nil {
		return err
	}
	return CheckNoWrite(name, text)
}

// CheckNoWrite refuses text holding INSERT, UPDATE or DELETE as a bare word,
// or REPLACE not followed by "(" (the replace() function), outside string
// literals, quoted identifiers and comments. It also refuses
// load_extension, bare or quoted: loading an extension is not undone by a
// rollback. A read statement never needs them, so a false positive only
// refuses an odd read.
func CheckNoWrite(name, text string) error {
	var toks []sqlscan.Token
	sqlscan.Scan(sqlscan.SQLite(), text, func(t sqlscan.Token) bool {
		if !isBlank(text, t) {
			toks = append(toks, t)
		}
		return true
	})
	for i, t := range toks {
		if t.Kind == sqlscan.QuotedIdent && asciiUpper(quotedBody(text[t.Start:t.End])) == "LOAD_EXTENSION" {
			return driver.Wrap(driver.ErrReadOnly, name, "query", errLoadExtension)
		}
		if t.Kind != sqlscan.Word {
			continue
		}
		switch asciiUpper(text[t.Start:t.End]) {
		case "LOAD_EXTENSION":
			return driver.Wrap(driver.ErrReadOnly, name, "query", errLoadExtension)
		case "INSERT", "UPDATE", "DELETE":
			return driver.Wrap(driver.ErrReadOnly, name, "query", errWritableCTE)
		case "REPLACE":
			if i+1 >= len(toks) || text[toks[i+1].Start:toks[i+1].End] != "(" {
				return driver.Wrap(driver.ErrReadOnly, name, "query", errWritableCTE)
			}
		}
	}
	return nil
}
