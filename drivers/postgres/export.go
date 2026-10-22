package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

var _ driver.Exporter = (*conn)(nil)

// ExportFormats lists the COPY formats Export writes.
func (*conn) ExportFormats() []string { return []string{"csv", "binary"} }

// Export streams the result of one SELECT-like statement to w with
// COPY (<query>) TO STDOUT, in constant memory.
//
// COPY takes no bind parameters, so q.Params must be empty, and it cannot
// limit rows, so q.Limit must be 0. The query text is embedded in the COPY
// statement, so exportQuery first proves it is one self-contained statement
// that cannot close the parenthesis or append COPY options (TO PROGRAM …).
// Export always runs in BEGIN READ ONLY (whatever the connection mode), with
// standard_conforming_strings forced on so the server lexes the text the way
// exportQuery did, and ends in ROLLBACK. Because READ ONLY does not stop
// server-side file access, every export also gets the read-only query path's
// refusals (serverAccessRefusal). ctx cancels the copy.
func (c *conn) Export(ctx context.Context, q driver.Query, format string, w io.Writer) (err error) {
	defer func() { err = c.named(err) }()
	const op = "export"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if !c.flavor.has(driver.CapExporter) {
		return c.flavor.unsupported(op, driver.CapExporter)
	}
	var opts string
	switch format {
	case "csv":
		opts = "FORMAT csv, HEADER true"
	case "binary":
		opts = "FORMAT binary"
	default:
		return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("unknown export format %q (have csv, binary)", format))
	}
	if len(q.Params) > 0 {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("COPY takes no bind parameters; inline the values"))
	}
	if q.Limit > 0 {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("COPY cannot limit rows; put LIMIT in the query"))
	}
	inner, err := exportQuery(q.Text)
	if err != nil {
		return driver.Wrap(driver.ErrUnsupported, Name, op, err)
	}
	// BEGIN READ ONLY does not stop server-side file access, so every export
	// gets the read-only path's refusals, whatever the connection's mode.
	if err := serverAccessRefusal(op, inner); err != nil {
		return err
	}
	if err := flavorRefusal(c.flavor, op, inner); err != nil {
		return err
	}
	// The newline ends a trailing line comment, so it cannot swallow ")".
	stmt := "COPY (" + inner + "\n) TO STDOUT WITH (" + opts + ")"

	err = withReadOnlyTx(ctx, c.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select set_config('standard_conforming_strings', 'on', true)"); err != nil {
			return err
		}
		if q.Namespace != "" {
			if _, err := tx.Exec(ctx, "select set_config('search_path', $1, true)", quoteIdent(q.Namespace)); err != nil {
				return err
			}
		}
		// Defense in depth: the server parses the inner text on its own. Text
		// that closes our parenthesis or holds a second statement does not
		// parse alone, whatever the scan above concluded.
		sd, err := tx.Conn().Prepare(ctx, "", inner)
		if err != nil {
			return err
		}
		if len(sd.ParamOIDs) > 0 {
			return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("COPY takes no bind parameters; inline the values"))
		}
		if _, err := tx.Conn().PgConn().CopyTo(ctx, w, stmt); err != nil {
			var pe *pgconn.PgError
			if !errors.As(err, &pe) {
				// Cancelled, writer failed or socket broke: the connection may
				// be mid-COPY. Never hand it back to the pool.
				discardConn(tx)
			}
			return err
		}
		if txEnded(tx) { // fail closed: nothing may end the transaction
			discardConn(tx)
			return escapeError(true)
		}
		return nil
	})
	return mapError(ctx, op, err)
}

// exportQuery checks that text is exactly one SELECT, WITH, VALUES or TABLE
// statement that is safe to embed between "COPY (" and ")", and returns it
// without trailing semicolons. Every refusal is ErrUnsupported.
//
// It is a lexical scan with driverutil/sqlscan (standard strings, as Export
// forces standard_conforming_strings on): strings, quoted identifiers, dollar
// quotes and (nested) comments are skipped and must be terminated; parentheses must
// balance and never close below depth 0, so the text cannot close our
// parenthesis; and no semicolon may appear except after the statement.
//
// The text is checked twice, with PostgreSQL's dollar-quote tags (bytes >=
// 0x80 allowed) and with ASCII-only tags, and is refused unless both readings
// accept the same statement.
func exportQuery(text string) (string, error) {
	std := sqlscan.Postgres(true)
	ascii := std
	ascii.DollarTagHighBit = false
	inner, err := exportScan(text, ascii)
	if err != nil {
		return "", err
	}
	other, err := exportScan(text, std)
	if err != nil {
		return "", err
	}
	if other != inner {
		return "", exportRefusal("the statement reads differently under PostgreSQL's string modes; simplify the quoting")
	}
	return inner, nil
}

func exportRefusal(msg string) error {
	return driver.Wrap(driver.ErrUnsupported, Name, "export", errors.New(msg))
}

// exportScan is exportQuery's check under one dialect.
func exportScan(text string, d sqlscan.Dialect) (string, error) {
	refuse := func(msg string) (string, error) { return "", exportRefusal(msg) }
	depth, end := 0, -1 // end: index of the first statement-ending ';'
	refusal := ""
	sqlscan.Scan(d, text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		switch {
		case t.Kind == sqlscan.Space, t.Kind == sqlscan.Comment:
		case t.Kind == sqlscan.Unterminated && raw[0] == '/':
			refusal = "unterminated comment"
		case t.Kind == sqlscan.Unterminated && (raw[0] == 'E' || raw[0] == 'e'): // E'…'
			refusal = "unterminated quoted text"
		case t.Kind == sqlscan.Semicolon:
			if depth > 0 {
				refusal = "semicolon inside parentheses"
			} else if end < 0 {
				end = t.Start
			}
		case t.Kind == sqlscan.Punct && raw == ")":
			depth--
			if depth < 0 {
				refusal = "unbalanced parenthesis"
			} else if end >= 0 {
				refusal = "only one statement can be exported"
			}
		case end >= 0:
			refusal = "only one statement can be exported"
		case t.Kind == sqlscan.Unterminated && raw[0] == '$':
			refusal = "unterminated dollar quote"
		case t.Kind == sqlscan.Unterminated:
			refusal = "unterminated quoted text"
		case t.Kind == sqlscan.Punct && raw == "(":
			depth++
		}
		return refusal == ""
	})
	if refusal != "" {
		return refuse(refusal)
	}
	if depth != 0 {
		return refuse("unbalanced parenthesis")
	}
	inner := text
	if end >= 0 {
		inner = text[:end]
	}
	switch kw := leadingKeyword(inner); {
	case isSelectLike(kw), kw == "WITH":
		return inner, nil
	default:
		return refuse("only a single SELECT, WITH, VALUES or TABLE statement can be exported")
	}
}
