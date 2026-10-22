package dynamodb

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// refusedWords are the bare words a read-only statement may not hold
// anywhere outside a string or a quoted identifier: every PartiQL write
// verb and clause DynamoDB knows (INSERT, UPDATE, DELETE, SET, REMOVE,
// RETURNING), EXISTS (the condition check of a transaction or batch), and
// verbs of other dialects so that a future DynamoDB extension fails
// closed. An attribute that shares one of these names must be quoted
// ("set").
var refusedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "UPSERT": true, "MERGE": true,
	"SET": true, "REMOVE": true, "RETURNING": true, "EXISTS": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true,
	"EXEC": true, "EXECUTE": true, "CALL": true, "BEGIN": true, "COMMIT": true, "ROLLBACK": true,
}

// checkReadOnly is the read-only guard for a PartiQL statement. The
// driver sends every query with ExecuteStatement, one statement per call,
// never as a batch or a transaction; the guard checks the text that is
// sent and fails closed:
//
//   - the statement must lex (strings, quoted identifiers and comments
//     terminated, no nested block comment, no stray non-ASCII byte);
//   - its first token must be the bare word SELECT;
//   - ";" may appear only as the last token;
//   - no bare word in refusedWords may appear anywhere;
//   - no Ion literal (`…`) may appear: the guard does not parse Ion.
//
// Anything else is ErrReadOnly. A PartiQL SELECT cannot write in
// DynamoDB.
func checkReadOnly(stmt string) error {
	toks, err := lex(stmt)
	if err != nil {
		return refuse(fmt.Errorf("cannot classify the statement: %w", err))
	}
	if len(toks) == 0 {
		return refuse(errors.New("empty statement"))
	}
	if !toks[0].isWord("SELECT") {
		return refuse(errors.New("only a SELECT statement is allowed on a read-only call"))
	}
	for i, t := range toks {
		switch {
		case t.kind == tokPunct && t.text == ";" && i != len(toks)-1:
			return refuse(errors.New("only one statement is allowed on a read-only call"))
		case t.kind == tokWord && refusedWords[strings.ToUpper(t.text)]:
			return refuse(fmt.Errorf("%s is not allowed on a read-only call (quote an attribute of that name)", strings.ToUpper(t.text)))
		case t.kind == tokIon:
			return refuse(errors.New("an Ion literal is not allowed on a read-only call"))
		}
	}
	return nil
}

func refuse(err error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", err)
}
